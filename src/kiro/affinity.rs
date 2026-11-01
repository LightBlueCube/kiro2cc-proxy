//! 会话亲和：同一会话尽量路由到同一账号，命中上游 prompt cache。
//!
//! 实测（2026-11-01，`e2e/p0-identity.mjs`、`e2e/p0-ttl.mjs`）：上游 prompt cache 按
//! 「账号 + 内容前缀」寻址，与 conversationId 无关；TTL 5 分钟，命中即续期。因此：
//! - 绑定只是偏好：账号能不能用完全由选号决定（[`MultiTokenManager::acquire_for_request`]），
//!   偏好账号在候选集里才会被选中
//! - 绑定空闲 5 分钟即失效：上游缓存已过期，继续绑定只会妨碍负载均衡
//! - 没有绑定的会话（新会话、子代理）按前缀族找 5 分钟内处理过同一前缀的账号，
//!   命中其它会话已写入的系统提示 + 工具定义缓存。前缀族全局共享、不按租户隔离：
//!   它只影响选哪个账号，不暴露内容
//!
//! | 事件                                   | 绑定                         |
//! |----------------------------------------|------------------------------|
//! | 无有效绑定                             | 绑定到本次账号               |
//! | 选号落在绑定账号（`Preferred`）        | 续期                         |
//! | 绑定账号本次请求内被避让（`Spilled`）  | 保持，不续期                 |
//! | 绑定账号不在候选集（`Fresh`）          | 改绑到本次账号               |
//! | 绑定账号上游限流                       | 连续计数，满 3 次解绑        |
//! | 绑定账号完整产出响应                   | 限流计数清零、续期           |
//!
//! 本地限额（每账号 RPM / 并发）满不算限流：同一会话的并行请求（如 Claude Code 子代理）
//! 挤满绑定账号时只临时溢出到其它账号（`Spilled`），不影响绑定。
//!
//! 前缀族只记最近一次处理它的账号：溢出到别的账号后，那个账号同样写入了该前缀的缓存。
//!
//! [`MultiTokenManager::acquire_for_request`]: crate::kiro::token_manager::MultiTokenManager::acquire_for_request

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use parking_lot::Mutex;

use crate::kiro::token_manager::Placement;

/// 上游 prompt cache 的 TTL（命中即续期）：绑定空闲超过它就没有缓存可命中
const BINDING_IDLE_TTL: Duration = Duration::from_secs(5 * 60);
/// 绑定账号上游连续限流多少次后解绑。单次 429 多为端点级瞬时限流，端点桶封禁与
/// rotation_bias 已能让本次请求避让；过早解绑会丢掉整段已缓存的上下文
pub(crate) const THROTTLE_UNBIND_THRESHOLD: u32 = 3;
/// 绑定表容量上限：超出时先清理过期绑定，仍超出再淘汰最久未用的
const MAX_BINDINGS: usize = 100_000;
/// 前缀族表容量上限（淘汰规则同绑定表）
const MAX_PREFIX_FAMILIES: usize = 10_000;

/// 一次请求的亲和键
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RouteKeys {
    /// 会话（conversationId）
    pub conversation: String,
    /// 前缀族：内容前缀相同、可共享上游缓存的请求共有的摘要（无可共享前缀时为 `None`）
    pub prefix_family: Option<String>,
}

struct Binding {
    account: u64,
    last_used: Instant,
    upstream_throttles: u32,
}

/// 前缀族最近一次被处理的账号
struct FamilyHint {
    account: u64,
    last_used: Instant,
}

fn is_live(last_used: Instant, now: Instant) -> bool {
    now.saturating_duration_since(last_used) < BINDING_IDLE_TTL
}

/// 累计计数（管理页展示）
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct AffinityMetrics {
    /// 选号落在会话绑定账号的次数
    pub preferred: u64,
    /// 有效绑定被改到其它账号的次数（首次绑定不计）
    pub rebinds: u64,
    /// 无绑定的会话按前缀族落到已缓存该前缀的账号的次数
    pub prefix_hits: u64,
}

/// 会话（conversationId）→ 账号的亲和绑定，以及前缀族 → 最近账号的提示
#[derive(Default)]
pub struct Affinity {
    bindings: Mutex<HashMap<String, Binding>>,
    families: Mutex<HashMap<String, FamilyHint>>,
    preferred: AtomicU64,
    rebinds: AtomicU64,
    prefix_hits: AtomicU64,
}

impl Affinity {
    pub fn new() -> Self {
        Self::default()
    }

    /// 本次请求偏好的账号：会话的有效绑定，没有则取前缀族最近处理过的账号
    pub fn preferred(&self, keys: &RouteKeys) -> Option<u64> {
        self.preferred_at(keys, Instant::now())
    }

    /// 记录本次请求的选号结果（`placement` 相对于 [`Self::preferred`] 的返回值）
    pub fn on_routed(&self, keys: &RouteKeys, account: u64, placement: Placement) {
        self.on_routed_at(keys, account, placement, Instant::now());
    }

    /// 账号上游限流（HTTP 429 或流中途的限流帧）
    pub fn on_upstream_throttled(&self, conversation: &str, account: u64) {
        let mut bindings = self.bindings.lock();
        let Some(binding) = bindings
            .get_mut(conversation)
            .filter(|b| b.account == account)
        else {
            return;
        };
        binding.upstream_throttles += 1;
        if binding.upstream_throttles < THROTTLE_UNBIND_THRESHOLD {
            return;
        }
        bindings.remove(conversation);
        tracing::info!(
            conversation,
            account,
            "会话亲和解绑：绑定账号连续上游限流 {} 次",
            THROTTLE_UNBIND_THRESHOLD
        );
    }

    /// 账号完整产出了响应
    pub fn on_completed(&self, conversation: &str, account: u64) {
        self.on_completed_at(conversation, account, Instant::now());
    }

    pub fn metrics(&self) -> AffinityMetrics {
        AffinityMetrics {
            preferred: self.preferred.load(Ordering::Relaxed),
            rebinds: self.rebinds.load(Ordering::Relaxed),
            prefix_hits: self.prefix_hits.load(Ordering::Relaxed),
        }
    }

    fn preferred_at(&self, keys: &RouteKeys, now: Instant) -> Option<u64> {
        if let Some(account) = self.bound_account_at(&keys.conversation, now) {
            return Some(account);
        }
        let family = keys.prefix_family.as_deref()?;
        self.families
            .lock()
            .get(family)
            .filter(|hint| is_live(hint.last_used, now))
            .map(|hint| hint.account)
    }

    fn bound_account_at(&self, conversation: &str, now: Instant) -> Option<u64> {
        let mut bindings = self.bindings.lock();
        let binding = bindings.get(conversation)?;
        if is_live(binding.last_used, now) {
            return Some(binding.account);
        }
        bindings.remove(conversation);
        None
    }

    fn on_routed_at(&self, keys: &RouteKeys, account: u64, placement: Placement, now: Instant) {
        self.route_conversation(&keys.conversation, account, placement, now);

        let Some(family) = &keys.prefix_family else {
            return;
        };
        let mut families = self.families.lock();
        families.insert(
            family.clone(),
            FamilyHint {
                account,
                last_used: now,
            },
        );
        enforce_capacity(&mut families, MAX_PREFIX_FAMILIES, now, |h| h.last_used);
    }

    fn route_conversation(
        &self,
        conversation: &str,
        account: u64,
        placement: Placement,
        now: Instant,
    ) {
        let mut bindings = self.bindings.lock();
        let live = bindings
            .get_mut(conversation)
            .filter(|b| is_live(b.last_used, now));

        let Some(binding) = live else {
            // 没有有效绑定时，偏好只可能来自前缀族
            if placement == Placement::Preferred {
                self.prefix_hits.fetch_add(1, Ordering::Relaxed);
            }
            bindings.insert(
                conversation.to_string(),
                Binding {
                    account,
                    last_used: now,
                    upstream_throttles: 0,
                },
            );
            enforce_capacity(&mut bindings, MAX_BINDINGS, now, |b| b.last_used);
            return;
        };

        match placement {
            Placement::Preferred => {
                if binding.account == account {
                    binding.last_used = now;
                }
                self.preferred.fetch_add(1, Ordering::Relaxed);
            }
            Placement::Spilled => {}
            Placement::Fresh => {
                if binding.account != account {
                    self.rebinds.fetch_add(1, Ordering::Relaxed);
                }
                *binding = Binding {
                    account,
                    last_used: now,
                    upstream_throttles: 0,
                };
            }
        }
    }

    fn on_completed_at(&self, conversation: &str, account: u64, now: Instant) {
        if let Some(binding) = self
            .bindings
            .lock()
            .get_mut(conversation)
            .filter(|b| b.account == account)
        {
            binding.upstream_throttles = 0;
            binding.last_used = now;
        }
    }
}

fn enforce_capacity<V>(
    map: &mut HashMap<String, V>,
    max: usize,
    now: Instant,
    last_used: impl Fn(&V) -> Instant,
) {
    if map.len() <= max {
        return;
    }
    map.retain(|_, v| is_live(last_used(v), now));
    while map.len() > max {
        let Some(oldest) = map
            .iter()
            .min_by_key(|(_, v)| last_used(v))
            .map(|(k, _)| k.clone())
        else {
            break;
        };
        map.remove(&oldest);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const KEY: &str = "conversation-1";
    const FAMILY: &str = "family-1";

    fn keys(conversation: &str, family: Option<&str>) -> RouteKeys {
        RouteKeys {
            conversation: conversation.to_string(),
            prefix_family: family.map(str::to_string),
        }
    }

    fn session() -> RouteKeys {
        keys(KEY, None)
    }

    #[test]
    fn test_fresh_binds_and_preferred_returns_account() {
        let affinity = Affinity::new();
        assert_eq!(affinity.preferred(&session()), None);

        affinity.on_routed(&session(), 7, Placement::Fresh);
        assert_eq!(affinity.preferred(&session()), Some(7));
        assert_eq!(
            affinity.metrics(),
            AffinityMetrics::default(),
            "首次绑定不计改绑"
        );
    }

    #[test]
    fn test_binding_expires_after_idle_ttl_and_use_refreshes_it() {
        let affinity = Affinity::new();
        let t0 = Instant::now();
        affinity.on_routed_at(&session(), 7, Placement::Fresh, t0);

        let t4 = t0 + Duration::from_secs(4 * 60);
        assert_eq!(affinity.preferred_at(&session(), t4), Some(7));
        affinity.on_routed_at(&session(), 7, Placement::Preferred, t4);
        // 距首次绑定 8 分钟，但距上次使用 4 分钟：仍有效
        assert_eq!(
            affinity.preferred_at(&session(), t4 + Duration::from_secs(4 * 60)),
            Some(7)
        );
        assert_eq!(
            affinity.preferred_at(&session(), t4 + BINDING_IDLE_TTL),
            None
        );
    }

    #[test]
    fn test_spilled_keeps_binding_without_refresh() {
        let affinity = Affinity::new();
        let t0 = Instant::now();
        affinity.on_routed_at(&session(), 7, Placement::Fresh, t0);
        affinity.on_routed_at(
            &session(),
            8,
            Placement::Spilled,
            t0 + Duration::from_secs(60),
        );

        assert_eq!(
            affinity.preferred_at(&session(), t0 + Duration::from_secs(61)),
            Some(7)
        );
        assert_eq!(
            affinity.preferred_at(&session(), t0 + BINDING_IDLE_TTL),
            None
        );
    }

    #[test]
    fn test_fresh_on_other_account_counts_rebind() {
        let affinity = Affinity::new();
        affinity.on_routed(&session(), 7, Placement::Fresh);
        affinity.on_routed(&session(), 8, Placement::Fresh);

        assert_eq!(affinity.preferred(&session()), Some(8));
        assert_eq!(affinity.metrics().rebinds, 1);
    }

    #[test]
    fn test_consecutive_upstream_throttles_unbind() {
        let affinity = Affinity::new();
        affinity.on_routed(&session(), 7, Placement::Fresh);
        for _ in 1..THROTTLE_UNBIND_THRESHOLD {
            affinity.on_upstream_throttled(KEY, 7);
        }
        assert_eq!(affinity.preferred(&session()), Some(7));

        affinity.on_upstream_throttled(KEY, 7);
        assert_eq!(affinity.preferred(&session()), None);
    }

    #[test]
    fn test_completed_resets_throttle_streak() {
        let affinity = Affinity::new();
        affinity.on_routed(&session(), 7, Placement::Fresh);
        for _ in 1..THROTTLE_UNBIND_THRESHOLD {
            affinity.on_upstream_throttled(KEY, 7);
        }
        affinity.on_completed(KEY, 7);
        for _ in 1..THROTTLE_UNBIND_THRESHOLD {
            affinity.on_upstream_throttled(KEY, 7);
        }
        assert_eq!(affinity.preferred(&session()), Some(7));
    }

    #[test]
    fn test_reports_on_other_account_are_ignored() {
        let affinity = Affinity::new();
        affinity.on_routed(&session(), 7, Placement::Fresh);
        for _ in 1..THROTTLE_UNBIND_THRESHOLD {
            affinity.on_upstream_throttled(KEY, 7);
        }
        // 溢出到账号 8 的请求成功，不代表绑定账号恢复
        affinity.on_completed(KEY, 8);
        for _ in 0..THROTTLE_UNBIND_THRESHOLD {
            affinity.on_upstream_throttled(KEY, 8);
        }
        assert_eq!(affinity.preferred(&session()), Some(7));

        affinity.on_upstream_throttled(KEY, 7);
        assert_eq!(affinity.preferred(&session()), None);
    }

    #[test]
    fn test_unbound_session_prefers_family_account() {
        let affinity = Affinity::new();
        affinity.on_routed(&keys("a", Some(FAMILY)), 7, Placement::Fresh);

        let new_session = keys("b", Some(FAMILY));
        assert_eq!(affinity.preferred(&new_session), Some(7));
        affinity.on_routed(&new_session, 7, Placement::Preferred);

        assert_eq!(affinity.preferred(&keys("b", None)), Some(7), "已绑定");
        let metrics = affinity.metrics();
        assert_eq!(metrics.prefix_hits, 1);
        assert_eq!(metrics.preferred, 0, "前缀族命中不计入绑定命中");
    }

    #[test]
    fn test_binding_outranks_family() {
        let affinity = Affinity::new();
        affinity.on_routed(&keys("a", Some(FAMILY)), 7, Placement::Fresh);
        affinity.on_routed(&keys("b", Some(FAMILY)), 8, Placement::Fresh);

        // 前缀族最近落在 8，但 a 自身绑定 7
        assert_eq!(affinity.preferred(&keys("a", Some(FAMILY))), Some(7));
        assert_eq!(affinity.preferred(&keys("c", Some(FAMILY))), Some(8));
    }

    #[test]
    fn test_family_hint_expires_with_cache_ttl() {
        let affinity = Affinity::new();
        let t0 = Instant::now();
        affinity.on_routed_at(&keys("a", Some(FAMILY)), 7, Placement::Fresh, t0);

        let new_session = keys("b", Some(FAMILY));
        assert_eq!(
            affinity.preferred_at(&new_session, t0 + Duration::from_secs(4 * 60)),
            Some(7)
        );
        assert_eq!(
            affinity.preferred_at(&new_session, t0 + BINDING_IDLE_TTL),
            None
        );
    }

    #[test]
    fn test_unbound_session_landing_elsewhere_binds_without_hit() {
        // 前缀族账号不可用（不在候选集 / 被避让）：会话绑到实际账号，前缀族改指向它
        let affinity = Affinity::new();
        affinity.on_routed(&keys("a", Some(FAMILY)), 7, Placement::Fresh);

        let new_session = keys("b", Some(FAMILY));
        affinity.on_routed(&new_session, 8, Placement::Spilled);

        assert_eq!(affinity.preferred(&new_session), Some(8));
        assert_eq!(affinity.preferred(&keys("c", Some(FAMILY))), Some(8));
        let metrics = affinity.metrics();
        assert_eq!(metrics.prefix_hits, 0);
        assert_eq!(metrics.rebinds, 0);
    }

    #[test]
    fn test_capacity_evicts_least_recently_used() {
        let affinity = Affinity::new();
        let t0 = Instant::now();
        for i in 0..=MAX_BINDINGS {
            let at = t0 + Duration::from_micros(i as u64);
            affinity.on_routed_at(&keys(&format!("k{i}"), None), 1, Placement::Fresh, at);
        }
        let bindings = affinity.bindings.lock();
        assert_eq!(bindings.len(), MAX_BINDINGS);
        assert!(!bindings.contains_key("k0"), "最久未用的绑定应被淘汰");
    }
}
