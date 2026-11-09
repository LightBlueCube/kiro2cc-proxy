//! MultiTokenManager 核心：构造初始化、配置读取、每凭据 RPM 上限等基础能力。
use super::super::entry::{CredentialEntry, DisabledReason, HealthStatus};
use super::super::probe::AccountsCoolingDown;
use super::super::refresh::{is_token_expired, is_token_expiring_soon, refresh_token};
use super::super::trial::{self, TrialLease};
use super::super::types::{CallContext, MultiTokenManager, Placement, TOKEN_REFRESH_COOLDOWN};
use super::super::upstream_failures::UpstreamFailures;

use crate::http_client::ProxyConfig;
use crate::kiro::machine_id;
use crate::kiro::model::credentials::KiroCredentials;
use crate::model::config::Config;
use parking_lot::Mutex;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::time::Instant;
use tokio::sync::Mutex as TokioMutex;

impl MultiTokenManager {
    /// 创建多账号 Token 管理器
    ///
    /// # Arguments
    /// * `config` - 应用配置
    /// * `credentials` - 账号列表
    /// * `proxy` - 可选的代理配置
    /// * `credentials_path` - 账号文件路径（用于回写）
    /// * `is_multiple_format` - 是否为多账号格式（决定回写 JSON 形状：数组 vs 单对象，不再影响是否回写）
    pub fn new(
        config: Config,
        credentials: Vec<KiroCredentials>,
        proxy: Option<ProxyConfig>,
        credentials_path: Option<PathBuf>,
        is_multiple_format: bool,
    ) -> anyhow::Result<Self> {
        // 计算当前最大 ID，为没有 ID 的账号分配新 ID
        //
        // 注意：不能只看当前 credentials 列表的最大 ID —— 账号删除后其 ID 会从列表中消失，
        // 若后续新增账号仅按“当前列表最大值 + 1”分配，会复用已删除账号曾用过的 ID，
        // 导致新账号继承该 ID 下遗留的历史用量/失败/限流日志（表现为“从未使用的新账号却有历史请求记录”）。
        // 因此需要额外加载持久化的历史最大 ID 计数器，取二者较大值，确保 ID 只增不减、永不复用。
        let max_existing_id = credentials.iter().filter_map(|c| c.id).max().unwrap_or(0);
        let persisted_max_id = Self::load_id_counter_from_path(credentials_path.as_deref());
        let starting_max_id = max_existing_id.max(persisted_max_id);
        // checked_add 防御：ID 计数器逼近 u64::MAX 时报错而非静默回绕（cr-result C7）
        let mut next_id = starting_max_id
            .checked_add(1)
            .expect("credential id 计数器溢出（u64::MAX），credentials.json 异常");
        let mut has_new_ids = false;
        let mut has_new_machine_ids = false;
        let mut has_new_profile_arns = false;
        let config_ref = &config;

        let entries: Vec<CredentialEntry> = credentials
            .into_iter()
            .map(|mut cred| {
                cred.canonicalize_auth_method();
                let id = cred.id.unwrap_or_else(|| {
                    let id = next_id;
                    next_id = next_id
                        .checked_add(1)
                        .expect("credential id 计数器溢出（u64::MAX），请检查 credentials.json 中的 id 字段");
                    cred.id = Some(id);
                    has_new_ids = true;
                    id
                });
                if cred.machine_id.is_none() {
                    cred.machine_id =
                        Some(machine_id::generate_from_credentials(&cred, config_ref));
                    has_new_machine_ids = true;
                }
                // 存量账号补全：旧版本添加的 social/idc 账号可能没有 profileArn，
                // 启动加载时按账号类型补全并标记写回配置文件
                if cred.fill_missing_profile_arn() {
                    has_new_profile_arns = true;
                }
                CredentialEntry {
                    id,
                    credentials: cred.clone(),
                    failure_count: 0,
                    refresh_failure_count: 0,
                    disabled: cred.disabled, // 从配置文件读取 disabled 状态
                    // 暂定 Manual；load_stats() 会用持久化的真实原因覆盖
                    // （额度耗尽/连续失败也会被 persist_credentials 写成 disabled: true，
                    //   若在此直接认定 Manual，额度耗尽账号将无法跨月自动恢复）
                    disabled_reason: if cred.disabled {
                        Some(DisabledReason::Manual)
                    } else {
                        None
                    },
                    success_count: 0,
                    last_used_at: None,
                    throttle_count: 0,
                    upstream_failures: UpstreamFailures::new(Instant::now()),
                    unreachable: Default::default(),
                    probe: Default::default(),
                    awaiting_verification: false,
                    trial: Default::default(),
                    model_refusals: Default::default(),
                    last_throttled_wall: None,
                    last_refreshed_at: None,
                    rotation_bias: 0,
                    quota_exhausted_at: None,
                    last_error: None,
                }
            })
            .collect();

        // 检测重复 ID
        let mut seen_ids = std::collections::HashSet::new();
        let mut duplicate_ids = Vec::new();
        for entry in &entries {
            if !seen_ids.insert(entry.id) {
                duplicate_ids.push(entry.id);
            }
        }
        if !duplicate_ids.is_empty() {
            anyhow::bail!("检测到重复的账号 ID: {:?}", duplicate_ids);
        }

        // 选择初始账号：优先级最高（priority 最小）的账号，无账号时为 0
        let initial_id = entries
            .iter()
            .min_by_key(|e| e.credentials.priority)
            .map(|e| e.id)
            .unwrap_or(0);

        // 历史最大 ID = 本次结束后 entries 中的最大值 与 持久化计数器 的较大值。
        // 二者缺一不可：entries 为空时需兜底 starting_max_id；entries 非空但其账号
        // 均携带小于 persisted_max_id 的显式 id 时（如本次重启后仅剩 #1，但磁盘计数器
        // 已因此前存在过 #2 而记录为 2），entries.max() 本身小于 starting_max_id，
        // 仍需与其取较大值，否则会丢失磁盘上记录的历史高位 ID，导致 ID 复用。
        let final_max_id = entries
            .iter()
            .map(|e| e.id)
            .max()
            .unwrap_or(0)
            .max(starting_max_id);

        let load_balancing_mode = config.load_balancing_mode.clone();
        let max_rpm = config.max_rpm_per_credential;
        let soft_rpm = config.soft_rpm_per_credential;
        let soft_rpm_spill = config.soft_rpm_spill_to_lower_priority;
        let max_concurrency = config.max_concurrency_per_credential;
        let manager = Self {
            config,
            proxy,
            entries: Mutex::new(entries),
            current_id: Mutex::new(initial_id),
            refresh_lock: TokioMutex::new(()),
            credential_identity_lock: TokioMutex::new(()),
            credential_admin_lock: TokioMutex::new(()),
            credentials_path,
            is_multiple_format: AtomicBool::new(is_multiple_format),
            load_balancing_mode: Mutex::new(load_balancing_mode),
            last_stats_save_at: Mutex::new(None),
            stats_dirty: AtomicBool::new(false),
            rr_counter: AtomicU64::new(0),
            persist_lock: Mutex::new(()),
            next_id_counter: AtomicU64::new(final_max_id),
            max_rpm_per_credential: AtomicU32::new(max_rpm),
            soft_rpm_per_credential: AtomicU32::new(soft_rpm),
            soft_rpm_spill_to_lower_priority: AtomicBool::new(soft_rpm_spill),
            ignore_health: AtomicBool::new(false),
            max_concurrency_per_credential: AtomicU32::new(max_concurrency),
            model_support: Mutex::new(HashMap::new()),
            model_support_refresh_lock: TokioMutex::new(()),
        };

        // 持久化历史最大 ID 计数器（即使本次没有新增账号，也要确保计数器文件与内存一致，
        // 避免文件丢失/首次运行时缺失导致回退到仅按当前列表推算）
        manager.save_id_counter_at_least(final_max_id);

        // 如果有新分配的 ID、新生成的 machineId 或补全的 profileArn，立即持久化到配置文件
        if has_new_ids || has_new_machine_ids || has_new_profile_arns {
            if let Err(e) = manager.persist_credentials() {
                tracing::warn!("补全账号 ID/machineId/profileArn 后持久化失败: {}", e);
            } else {
                tracing::info!("已补全账号 ID/machineId/profileArn 并写回配置文件");
            }
        }

        // 加载持久化的统计数据（success_count, last_used_at, disabled_reason）
        manager.load_stats();

        // 启动即检查：跨自然月后自动恢复因额度用尽被禁用的账号
        manager.recover_expired_quota_disables();

        Ok(manager)
    }

    /// 获取配置的引用
    pub fn config(&self) -> &Config {
        &self.config
    }

    /// 获取每账号 RPM 上限（运行时热切换后的当前值；0 = 不限）
    pub fn max_rpm_per_credential(&self) -> u32 {
        self.max_rpm_per_credential.load(Ordering::Relaxed)
    }

    /// 热切换每账号 RPM 上限（0 = 不限），仅影响运行时，持久化由调用方负责
    pub fn set_max_rpm_per_credential(&self, value: u32) {
        self.max_rpm_per_credential.store(value, Ordering::Relaxed);
    }

    /// 获取每账号 RPM 软上限（运行时热切换后的当前值；0 = 关闭）
    pub fn soft_rpm_per_credential(&self) -> u32 {
        self.soft_rpm_per_credential.load(Ordering::Relaxed)
    }

    /// 热切换每账号 RPM 软上限（0 = 关闭），仅影响运行时，持久化由调用方负责
    pub fn set_soft_rpm_per_credential(&self, value: u32) {
        self.soft_rpm_per_credential.store(value, Ordering::Relaxed);
    }

    /// 优先级模式下最高一档全部达到软上限时，新会话是否落到下一档
    pub fn soft_rpm_spill_to_lower_priority(&self) -> bool {
        self.soft_rpm_spill_to_lower_priority
            .load(Ordering::Relaxed)
    }

    /// 热切换软上限是否允许溢出到下一档，仅影响运行时，持久化由调用方负责
    pub fn set_soft_rpm_spill_to_lower_priority(&self, enabled: bool) {
        self.soft_rpm_spill_to_lower_priority
            .store(enabled, Ordering::Relaxed);
    }

    /// 获取每账号并发上限（运行时热切换后的当前值；0 = 不限）
    pub fn max_concurrency_per_credential(&self) -> u32 {
        self.max_concurrency_per_credential.load(Ordering::Relaxed)
    }

    /// 热切换每账号并发上限（0 = 不限），仅影响运行时，持久化由调用方负责
    pub fn set_max_concurrency_per_credential(&self, value: u32) {
        self.max_concurrency_per_credential
            .store(value, Ordering::Relaxed);
    }

    /// 获取当前活动账号的克隆（仅测试使用）
    #[cfg(test)]
    pub fn credentials(&self) -> KiroCredentials {
        let entries = self.entries.lock();
        let current_id = *self.current_id.lock();
        entries
            .iter()
            .find(|e| e.id == current_id)
            .map(|e| e.credentials.clone())
            .unwrap_or_default()
    }

    /// 获取账号总数
    pub fn total_count(&self) -> usize {
        self.entries.lock().len()
    }

    /// 获取可用账号数量
    pub fn available_count(&self) -> usize {
        self.entries.lock().iter().filter(|e| !e.disabled).count()
    }

    /// 返回当前未禁用账号的 id 列表
    pub fn credential_ids(&self) -> Vec<u64> {
        self.entries
            .lock()
            .iter()
            .filter(|e| !e.disabled)
            .map(|e| e.id)
            .collect()
    }

    /// 本次可选的账号集合（负载均衡在其中挑选）
    ///
    /// 先按禁用、白名单、订阅等级、模型可用性硬性过滤，再优先取健康状态不为 Unhealthy、
    /// 该模型未在拒答冷却期、且（待验证时）试验名额空闲的账号。全部不满足时退回硬性过滤后的
    /// 全集，但其中冷却中的账号只放行试探时间已到的（见 `probe` 模块），名额被占的待验证账号
    /// 暂不放行；一个都放不出时返回 [`AccountsCoolingDown`]。
    /// 只因拒答冷却的账号照常放行，把上游说明原文返回给客户端。
    /// 开启无视健康开关（[`Self::ignore_health`]）后，账号冷却与试验名额不再排除任何账号。
    fn candidate_pool<'a>(
        &self,
        entries: &'a [CredentialEntry],
        model: Option<&str>,
        allowed_ids: &[u64],
        now: Instant,
    ) -> Result<Vec<&'a CredentialEntry>, AccountsCoolingDown> {
        let is_opus = model
            .map(|m| m.to_lowercase().contains("opus"))
            .unwrap_or(false);

        let available: Vec<_> = entries
            .iter()
            .filter(|e| {
                if e.disabled {
                    return false;
                }
                // 账号 ID 白名单过滤（空列表表示不限制）
                if !allowed_ids.is_empty() && !allowed_ids.contains(&e.id) {
                    return false;
                }
                // 如果是 opus 模型，需要检查订阅等级
                if is_opus && !e.credentials.supports_opus() {
                    return false;
                }
                true
            })
            .collect();

        // 按账号实际可用模型过滤（探测结果未知时不排除，详见 prefer_model_supporters）
        let available = self.prefer_model_supporters(available, model);

        let ignore_health = self.ignore_health();
        let cooling = |e: &CredentialEntry| {
            !ignore_health && Self::compute_health_at(e, now) == HealthStatus::Unhealthy
        };
        let trial_busy =
            |e: &CredentialEntry| !ignore_health && e.awaiting_verification && e.trial.is_busy(now);
        let preferred: Vec<_> = available
            .iter()
            .filter(|e| !cooling(e) && !Self::model_restricted_at(e, model, now) && !trial_busy(e))
            .copied()
            .collect();
        if !preferred.is_empty() {
            return Ok(preferred);
        }

        // 暂时放不出的账号还要等多久：冷却中等试探时间，名额被占的等一个短间隔再重选
        let wait = |e: &CredentialEntry| {
            if cooling(e) {
                e.probe.wait_remaining(now)
            } else {
                trial_busy(e).then_some(trial::RECHECK)
            }
        };
        let ready: Vec<_> = available
            .iter()
            .filter(|e| wait(e).is_none())
            .copied()
            .collect();
        if !ready.is_empty() {
            return Ok(ready);
        }
        match available.iter().filter_map(|e| wait(e)).min() {
            Some(retry_after) => Err(AccountsCoolingDown::new(retry_after)),
            // 硬性过滤后一个账号都没有，由调用方说明原因
            None => Ok(Vec::new()),
        }
    }

    /// 选号后占住名额，须与选号在同一次持锁内完成，并发选号才不会同时选中同一个名额
    ///
    /// 冷却中的账号（候选账号全部冷却时的试探）占试探名额；待验证的账号占试验名额，
    /// 返回的租约随调用上下文释放。无视健康开关开启时不占名额。
    fn claim_admission(
        &self,
        entries: &mut [CredentialEntry],
        id: u64,
        now: Instant,
    ) -> Option<TrialLease> {
        if self.ignore_health() {
            return None;
        }
        let entry = entries.iter_mut().find(|e| e.id == id)?;
        match Self::compute_health_at(entry, now) {
            HealthStatus::Unhealthy => {
                entry.probe.claim(now);
                None
            }
            HealthStatus::PendingVerification => Some(entry.trial.claim(now)),
            _ => None,
        }
    }

    /// 根据负载均衡模式选择下一个账号
    ///
    /// - priority 模式：在优先级最高（priority 最小）的一档可用账号内轮询
    /// - balanced 模式：在全部可用账号内轮询
    ///
    /// # 参数
    /// - `model`: 可选的模型名称，用于过滤支持该模型的账号（如 opus 模型需要付费订阅）
    fn select_next_credential(
        &self,
        model: Option<&str>,
        allowed_ids: &[u64],
    ) -> Result<Option<(u64, KiroCredentials, Option<TrialLease>)>, AccountsCoolingDown> {
        let mut entries = self.entries.lock();
        let now = Instant::now();
        let selected = {
            let pool = self.candidate_pool(&entries, model, allowed_ids, now)?;
            self.pick_from_pool(&pool, now)
        };
        Ok(selected.map(|(id, credentials)| {
            let trial = self.claim_admission(&mut entries, id, now);
            (id, credentials, trial)
        }))
    }

    /// 按负载均衡模式在候选集里挑一个账号
    fn pick_from_pool(
        &self,
        pool: &[&CredentialEntry],
        now: Instant,
    ) -> Option<(u64, KiroCredentials)> {
        if pool.is_empty() {
            return None;
        }
        // 降级账号相当于优先级 +0.5：同优先级内最后被选中，仍先于下一优先级
        // （待验证账号不降档：否则总有别的账号排在前面，永远等不到验证；
        // 无视健康开关开启时降级也不降档）
        let ignore_health = self.ignore_health();
        let degraded = |e: &CredentialEntry| {
            !ignore_health && Self::compute_health_at(e, now) == HealthStatus::Degraded
        };

        let mode = self.load_balancing_mode.lock().clone();
        let mode = mode.as_str();

        match mode {
            "balanced" => {
                // 先排除降级账号（全部降级时不排除），再 Round-Robin + rotation_bias：
                // 优先选 bias 最小的子集，再 round-robin
                let not_degraded: Vec<&CredentialEntry> =
                    pool.iter().filter(|e| !degraded(e)).copied().collect();
                let pool = if not_degraded.is_empty() {
                    pool
                } else {
                    &not_degraded
                };
                let min_bias = pool.iter().map(|e| e.rotation_bias).min().unwrap_or(0);
                let low_bias: Vec<&CredentialEntry> = pool
                    .iter()
                    .filter(|e| e.rotation_bias == min_bias)
                    .copied()
                    .collect();
                let idx = self.rr_counter.fetch_add(1, Ordering::Relaxed) as usize;
                let entry = low_bias[idx % low_bias.len()];
                Some((entry.id, entry.credentials.clone()))
            }
            _ => {
                // priority 模式：取顺位最高的一档（优先级 ×2，降级再 +1），
                // 档内按 rotation_bias 排序后 round-robin
                let rank = |e: &CredentialEntry| {
                    u64::from(e.credentials.priority) * 2 + u64::from(degraded(e))
                };
                let min_rank = pool.iter().map(|e| rank(e)).min()?;
                let top_tier: Vec<&CredentialEntry> = pool
                    .iter()
                    .filter(|e| rank(e) == min_rank)
                    .copied()
                    .collect();
                if top_tier.len() == 1 {
                    Some((top_tier[0].id, top_tier[0].credentials.clone()))
                } else {
                    let min_bias = top_tier.iter().map(|e| e.rotation_bias).min().unwrap_or(0);
                    let low_bias: Vec<&CredentialEntry> = top_tier
                        .iter()
                        .filter(|e| e.rotation_bias == min_bias)
                        .copied()
                        .collect();
                    let idx = self.rr_counter.fetch_add(1, Ordering::Relaxed) as usize;
                    let entry = low_bias[idx % low_bias.len()];
                    Some((entry.id, entry.credentials.clone()))
                }
            }
        }
    }

    /// 获取 API 调用上下文
    ///
    /// 返回绑定了 id、credentials 和 token 的调用上下文
    /// 确保整个 API 调用过程中使用一致的账号信息
    ///
    /// 如果 Token 过期或即将过期，会自动刷新
    /// Token 刷新失败时会尝试下一个可用账号（不计入失败次数）
    ///
    /// # 参数
    /// - `model`: 可选的模型名称，用于过滤支持该模型的账号（如 opus 模型需要付费订阅）
    pub async fn acquire_context(&self, model: Option<&str>) -> anyhow::Result<CallContext> {
        // 跨自然月后，额度已重置，先把此前判定耗尽的账号放回可用池
        self.recover_expired_quota_disables();

        let total = self.total_count();
        // Token 刷新失败的账号本次不再选
        let mut failed_ids: Vec<u64> = Vec::new();

        loop {
            if failed_ids.len() >= total {
                anyhow::bail!(
                    "所有账号均无法获取有效 Token（可用: {}/{}）",
                    self.available_count(),
                    total
                );
            }

            // 两种模式都每次重新选号（priority 在最高一档内轮询，balanced 在全部可用账号内轮询）。
            // 没有可用账号时不自动启用任何被禁用的账号，只能手动恢复
            let remaining: Vec<u64> = if failed_ids.is_empty() {
                Vec::new()
            } else {
                self.credential_ids()
                    .into_iter()
                    .filter(|id| !failed_ids.contains(id))
                    .collect()
            };
            let Some((id, credentials, trial)) = self.select_next_credential(model, &remaining)?
            else {
                // describe_unavailable 内部会获取 entries 锁，
                // 因此必须在任何 entries 锁作用域之外调用，否则死锁
                anyhow::bail!("{}", self.describe_unavailable(model, &[]));
            };
            *self.current_id.lock() = id;

            match self.try_ensure_token(id, &credentials).await {
                Ok(ctx) => return Ok(CallContext { trial, ..ctx }),
                Err(e) => {
                    tracing::warn!("账号 #{} Token 刷新失败，尝试下一个账号: {}", id, e);
                    self.report_refresh_error(id, &e);
                    failed_ids.push(id);
                }
            }
        }
    }

    /// 带账号 ID 白名单的调用上下文获取
    ///
    /// 与 acquire_context 逻辑相同，但只在 allowed_ids 指定的账号中选择。
    /// 白名单内所有账号均不可用时直接返回错误，不回退到全局池。
    pub async fn acquire_context_filtered(
        &self,
        model: Option<&str>,
        allowed_ids: &[u64],
    ) -> anyhow::Result<CallContext> {
        if allowed_ids.is_empty() {
            return self.acquire_context(model).await;
        }

        // 跨自然月后，额度已重置，先把此前判定耗尽的账号放回可用池
        self.recover_expired_quota_disables();

        let mut tried_ids: Vec<u64> = Vec::new();

        loop {
            if tried_ids.len() >= allowed_ids.len() {
                anyhow::bail!("{}", self.describe_unavailable(model, allowed_ids));
            }

            // 从白名单中排除已尝试过的账号
            let effective_ids: Vec<u64> = allowed_ids
                .iter()
                .filter(|id| !tried_ids.contains(id))
                .copied()
                .collect();

            let (id, credentials, trial) = {
                match self.select_next_credential(model, &effective_ids)? {
                    Some(selected) => selected,
                    None => {
                        anyhow::bail!("{}", self.describe_unavailable(model, allowed_ids));
                    }
                }
            };

            match self.try_ensure_token(id, &credentials).await {
                Ok(ctx) => return Ok(CallContext { trial, ..ctx }),
                Err(e) => {
                    tracing::warn!("绑定账号 #{} Token 刷新失败，尝试下一个: {}", id, e);

                    self.report_refresh_error(id, &e);

                    tried_ids.push(id);
                }
            }
        }
    }

    /// 在排除 `avoid_ids` 的前提下选账号
    ///
    /// 用于同一请求内的重试：绑定账号刚被限流时换个账号完成本次调用。
    /// 排除后无账号可用时回退到不排除的选择逻辑，保证不会因避让而彻底失败。
    async fn acquire_context_avoiding(
        &self,
        model: Option<&str>,
        allowed_ids: &[u64],
        avoid_ids: &[u64],
    ) -> anyhow::Result<CallContext> {
        let base_ids: Vec<u64> = if allowed_ids.is_empty() {
            self.entries.lock().iter().map(|e| e.id).collect()
        } else {
            allowed_ids.to_vec()
        };
        let remaining: Vec<u64> = base_ids
            .iter()
            .filter(|id| !avoid_ids.contains(id))
            .copied()
            .collect();

        if remaining.is_empty() {
            // 所有候选账号都已限流：回到原逻辑，由上层重试与退避处理
            return self.acquire_context_filtered(model, allowed_ids).await;
        }

        match self.acquire_context_filtered(model, &remaining).await {
            Ok(ctx) => Ok(ctx),
            Err(_) => self.acquire_context_filtered(model, allowed_ids).await,
        }
    }

    /// 为一次上游请求选账号，`preferred` 是会话亲和偏好的账号
    ///
    /// 偏好账号与负载均衡用同一个候选集（[`Self::candidate_pool`]）：在候选集里就用它，
    /// 否则按负载均衡选号，规则完全一致。`avoid_ids` 是本次请求内已经限流、本地限额满或
    /// 账号失败过的账号，重试时避开。`soft_full_ids` 是本地 RPM 已达软上限的账号，
    /// 只对新会话传入：新会话尽量不绑到这些账号（见 [`Self::soft_avoid_ids`]）。
    pub async fn acquire_for_request(
        &self,
        model: Option<&str>,
        allowed_ids: &[u64],
        preferred: Option<u64>,
        avoid_ids: &[u64],
        soft_full_ids: &[u64],
    ) -> anyhow::Result<(CallContext, Placement)> {
        let soft_avoid = self.soft_avoid_ids(model, allowed_ids, avoid_ids, soft_full_ids);
        let usable = |id: &u64| !avoid_ids.contains(id) && !soft_avoid.contains(id);
        if let Some(id) = preferred.filter(usable) {
            let claimed = {
                let mut entries = self.entries.lock();
                let now = Instant::now();
                // 全部冷却且没到试探时间：交给下方负载均衡返回 AccountsCoolingDown
                let credentials = self
                    .candidate_pool(&entries, model, allowed_ids, now)
                    .unwrap_or_default()
                    .into_iter()
                    .find(|e| e.id == id)
                    .map(|e| e.credentials.clone());
                credentials
                    .map(|credentials| (credentials, self.claim_admission(&mut entries, id, now)))
            };
            if let Some((credentials, trial)) = claimed {
                match self.try_ensure_token(id, &credentials).await {
                    Ok(ctx) => return Ok((CallContext { trial, ..ctx }, Placement::Preferred)),
                    Err(e) => {
                        tracing::warn!("亲和账号 #{} Token 刷新失败，改选其它账号: {}", id, e);
                        self.report_refresh_error(id, &e);
                    }
                }
            }
        }

        let all_avoid: Vec<u64> = avoid_ids.iter().chain(&soft_avoid).copied().collect();
        let ctx = if all_avoid.is_empty() {
            self.acquire_context_filtered(model, allowed_ids).await?
        } else {
            self.acquire_context_avoiding(model, allowed_ids, &all_avoid)
                .await?
        };
        let placement = match preferred {
            Some(id) if id == ctx.id => Placement::Preferred,
            Some(id) if avoid_ids.contains(&id) => Placement::Spilled,
            _ => Placement::Fresh,
        };
        Ok((ctx, placement))
    }

    /// 新会话本次要避开的软上限已满账号
    ///
    /// balanced 模式或允许溢出到下一档时，避开全部已满账号。不允许溢出时只避开最高优先级
    /// 一档里已满的账号，整档都满就一个都不避开（留在这一档，由硬上限把关）。
    /// 避开后无号可选时 [`Self::acquire_context_avoiding`] 退回全集，所以全满时照常选号。
    fn soft_avoid_ids(
        &self,
        model: Option<&str>,
        allowed_ids: &[u64],
        avoid_ids: &[u64],
        soft_full_ids: &[u64],
    ) -> Vec<u64> {
        if soft_full_ids.is_empty() {
            return Vec::new();
        }
        let balanced = self.load_balancing_mode.lock().as_str() == "balanced";
        if balanced || self.soft_rpm_spill_to_lower_priority() {
            return soft_full_ids.to_vec();
        }

        let entries = self.entries.lock();
        let pool: Vec<&CredentialEntry> = self
            .candidate_pool(&entries, model, allowed_ids, Instant::now())
            .unwrap_or_default()
            .into_iter()
            .filter(|e| !avoid_ids.contains(&e.id))
            .collect();
        let Some(top_priority) = pool.iter().map(|e| e.credentials.priority).min() else {
            return Vec::new();
        };
        let top_tier: Vec<u64> = pool
            .iter()
            .filter(|e| e.credentials.priority == top_priority)
            .map(|e| e.id)
            .collect();
        if top_tier.iter().all(|id| soft_full_ids.contains(id)) {
            return Vec::new();
        }
        top_tier
            .into_iter()
            .filter(|id| soft_full_ids.contains(id))
            .collect()
    }

    /// 选择优先级最高的未禁用账号作为当前账号（内部方法）
    ///
    /// 纯粹按优先级选择，用于优先级变更后立即更新管理面板的「当前账号」
    pub(crate) fn select_highest_priority(&self) {
        let entries = self.entries.lock();
        let mut current_id = self.current_id.lock();

        // 选择优先级最高的未禁用账号（不排除当前账号）
        if let Some(best) = entries
            .iter()
            .filter(|e| !e.disabled)
            .min_by_key(|e| e.credentials.priority)
            && best.id != *current_id
        {
            tracing::info!(
                "优先级变更后切换账号: #{} -> #{}（优先级 {}）",
                *current_id,
                best.id,
                best.credentials.priority
            );
            *current_id = best.id;
        }
    }

    /// 尝试使用指定账号获取有效 Token
    ///
    /// 使用双重检查锁定模式，确保同一时间只有一个刷新操作
    ///
    /// # Arguments
    /// * `id` - 账号 ID，用于更新正确的条目
    /// * `credentials` - 账号信息
    async fn try_ensure_token(
        &self,
        id: u64,
        credentials: &KiroCredentials,
    ) -> anyhow::Result<CallContext> {
        // 第一次检查（无锁）：快速判断是否需要刷新
        let needs_refresh = is_token_expired(credentials) || is_token_expiring_soon(credentials);

        let creds = if needs_refresh {
            // 获取刷新锁，确保同一时间只有一个刷新操作
            let _guard = self.refresh_lock.lock().await;
            let _identity_guard = self.credential_identity_lock.lock().await;

            // 第二次检查：获取锁后重新读取账号，因为其他请求可能已经完成刷新
            let current_creds = {
                let entries = self.entries.lock();
                entries
                    .iter()
                    .find(|e| e.id == id)
                    .map(|e| e.credentials.clone())
                    .ok_or_else(|| anyhow::anyhow!("账号 #{} 不存在", id))?
            };

            if is_token_expired(&current_creds) || is_token_expiring_soon(&current_creds) {
                // 冷却期检查：仅对"即将过期"生效，已过期必须立即刷新
                let skip_for_cooldown = !is_token_expired(&current_creds) && {
                    let entries = self.entries.lock();
                    entries
                        .iter()
                        .find(|e| e.id == id)
                        .and_then(|e| e.last_refreshed_at)
                        .map(|t| t.elapsed() < TOKEN_REFRESH_COOLDOWN)
                        .unwrap_or(false)
                };
                if skip_for_cooldown {
                    tracing::debug!("Token 即将过期但在冷却期内（30s），跳过刷新");
                    current_creds
                } else {
                    // 确实需要刷新
                    let effective_proxy = current_creds.effective_proxy(self.proxy.as_ref());
                    let refreshed_creds =
                        refresh_token(&current_creds, &self.config, effective_proxy.as_ref())
                            .await?;

                    if is_token_expired(&refreshed_creds) {
                        anyhow::bail!("刷新后的 Token 仍然无效或已过期");
                    }

                    let committed_creds = self.commit_refreshed_credentials_if_identity_matches(
                        id,
                        &current_creds,
                        &refreshed_creds,
                    )?;

                    // 回写账号到文件（仅多账号格式），失败只记录警告
                    if let Err(e) = self.persist_credentials() {
                        tracing::warn!("Token 刷新后持久化失败（不影响本次请求）: {}", e);
                    }

                    committed_creds
                }
            } else {
                // 其他请求已经完成刷新，直接使用新账号
                tracing::debug!("Token 已被其他请求刷新，跳过刷新");
                current_creds
            }
        } else {
            credentials.clone()
        };

        let token = creds
            .access_token
            .clone()
            .ok_or_else(|| anyhow::anyhow!("没有可用的 accessToken"))?;

        Ok(CallContext {
            id,
            credentials: creds,
            token,
            trial: None,
        })
    }
}
