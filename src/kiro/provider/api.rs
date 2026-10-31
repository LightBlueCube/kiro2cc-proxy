//! Kiro 上游调用公开入口：call_api / call_api_stream / call_mcp（含 MCP 重试）。
use std::sync::Arc;
use std::time::Duration;

use tokio::time::{Instant, sleep};

use super::core::{KiroProvider, MAX_RETRIES_PER_CREDENTIAL, MAX_TOTAL_RETRIES};
use super::errors::LOCAL_LIMIT_QUEUE;
use super::retry::push_unique;
use crate::kiro::token_manager::AccountError;

/// 上游成功响应及其来历
pub struct ApiResponse {
    pub response: reqwest::Response,
    /// 实际服务本次请求的账号
    pub credential_id: u64,
    /// 历史推理签名被上游拒绝后，为拿到本响应对请求体做的删改
    pub signature_repair: SignatureRepair,
    /// 该账号的在途请求计数（未启用 RPM 监控时为 None）；须保存到响应体发完
    pub credential_in_flight: Option<crate::model::rpm::InFlightGuard>,
    /// 本响应走的上游路由；流内收到限流帧时交给 [`KiroProvider::report_stream_throttled`]
    pub route: UpstreamRoute,
}

/// 读完响应体才能判定的上游结果，回报给 provider 记账（健康度、限流避让）
///
/// 由 handler 用 [`ApiResponse::route`] 构造；每种结果对一次响应最多回报一次。
pub struct UpstreamFeedback {
    provider: Arc<KiroProvider>,
    route: UpstreamRoute,
}

impl UpstreamFeedback {
    pub fn new(provider: Arc<KiroProvider>, route: UpstreamRoute) -> Self {
        Self { provider, route }
    }

    /// 流中途收到限流帧（参数为帧内文案）
    pub fn throttled(&self, message: &str) {
        self.provider.report_stream_throttled(&self.route, message);
    }

    /// 200 响应正常结束但没有任何内容（调用方已排除上下文过大导致的空响应）
    pub fn empty_response(&self) {
        tracing::warn!(
            credential_id = self.route.credential_id,
            endpoint = self.route.endpoint.as_str(),
            "上游空响应，计入账号健康度"
        );
        self.provider
            .token_manager
            .report_empty_response(self.route.credential_id);
    }

    /// 响应正常结束且有内容
    pub fn completed(&self) {
        self.provider
            .token_manager
            .report_response_completed(self.route.credential_id, self.route.model.as_deref());
    }

    /// 模型拒答（stopReason=content_filtered，参数为上游说明原文）：计入该账号该模型的连续拒答
    pub fn refused(&self, message: &str) {
        self.provider.token_manager.report_model_refused(
            self.route.credential_id,
            self.route.model.as_deref(),
            message,
        );
    }
}

/// 一次成功上游调用的路由信息（账号、端点、会话），流中途限流时据此记账避让
#[derive(Debug, Clone)]
pub struct UpstreamRoute {
    pub(super) credential_id: u64,
    pub(super) endpoint: crate::kiro::endpoint::EndpointName,
    pub(super) continuation_id: Option<String>,
    pub(super) model: Option<String>,
    /// 本次请求内第几次尝试（从 1 起）
    pub(super) attempt: u32,
}

/// 历史推理签名被拒后的请求体删改
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct SignatureRepair {
    /// 被上游点名拒绝、已从请求中删除的签名（删除后上游不再点名同一位置，确认是坏签名）
    pub rejected: Vec<String>,
    /// 逐个删除未能收敛，最终删除了全部历史推理
    pub stripped_all: bool,
}

impl KiroProvider {
    pub async fn call_api(
        &self,
        request_body: &str,
        is_compact: bool,
        bound_ids: &[u64],
    ) -> anyhow::Result<ApiResponse> {
        self.call_api_with_retry(request_body, false, is_compact, bound_ids)
            .await
    }

    /// 发送流式 API 请求
    ///
    /// 支持多账号故障转移：
    /// - 400 Bad Request: 直接返回错误，不计入账号失败
    /// - 401/403: 视为账号/权限问题，计入失败次数并允许故障转移
    /// - 402 MONTHLY_REQUEST_COUNT: 视为额度用尽，禁用账号并切换
    /// - 429/5xx/网络等瞬态错误: 重试但不禁用或切换账号（避免误把所有账号锁死）
    ///
    /// # Arguments
    /// * `request_body` - JSON 格式的请求体字符串
    /// * `is_compact` - 是否为 Claude Code `/compact` 压缩请求
    ///   （超时统一为 `UPSTREAM_TIMEOUT_SECS`，历史分档已移除）
    ///
    /// # Returns
    /// 返回原始的 HTTP Response，调用方负责处理流式数据
    pub async fn call_api_stream(
        &self,
        request_body: &str,
        is_compact: bool,
        bound_ids: &[u64],
    ) -> anyhow::Result<ApiResponse> {
        self.call_api_with_retry(request_body, true, is_compact, bound_ids)
            .await
    }

    /// 发送 MCP API 请求
    ///
    /// 用于 WebSearch 等工具调用。MCP 调用不涉及 `/compact` 压缩语义，
    /// 与普通请求共用统一超时（`UPSTREAM_TIMEOUT_SECS`）。
    ///
    /// # Arguments
    /// * `request_body` - JSON 格式的 MCP 请求体字符串
    ///
    /// # Returns
    /// 返回原始的 HTTP Response
    pub async fn call_mcp(
        &self,
        request_body: &str,
        bound_ids: &[u64],
    ) -> anyhow::Result<(reqwest::Response, u64)> {
        self.call_mcp_with_retry(request_body, bound_ids).await
    }

    /// 内部方法：带重试逻辑的 MCP API 调用
    pub(crate) async fn call_mcp_with_retry(
        &self,
        request_body: &str,
        bound_ids: &[u64],
    ) -> anyhow::Result<(reqwest::Response, u64)> {
        let effective_pool = if bound_ids.is_empty() {
            self.token_manager.total_count()
        } else {
            bound_ids.len()
        };
        let max_retries = (effective_pool * MAX_RETRIES_PER_CREDENTIAL).min(MAX_TOTAL_RETRIES);
        let small_pool = effective_pool <= 1;
        let mut last_error: Option<anyhow::Error> = None;

        let continuation_id = Self::extract_continuation_id_from_request(request_body);
        // 退避、限额与 429 的处理与 call_api_with_retry 同口径（见该函数注释）
        let mut throttled_in_request: Vec<u64> = Vec::new();
        let mut upstream_throttled: Vec<u64> = Vec::new();
        let mut queue_deadline: Option<Instant> = None;
        let mut throttled = false;
        let mut backoff: Option<Duration> = None;
        let mut throttle_rehits = 0;

        for attempt in 0..max_retries {
            if let Some(delay) = backoff.take() {
                sleep(delay).await;
            }
            // 获取调用上下文（MCP 不涉及模型选择，但同样应用 sticky 路由）
            let ctx = match self
                .token_manager
                .acquire_context_sticky(
                    None,
                    bound_ids,
                    continuation_id.as_deref(),
                    &throttled_in_request,
                )
                .await
            {
                Ok(c) => c,
                Err(e) => {
                    last_error = Some(e);
                    continue;
                }
            };

            if upstream_throttled.contains(&ctx.id) {
                sleep(Self::throttle_delay(throttle_rehits)).await;
                throttle_rehits += 1;
            }

            let can_switch = !small_pool && !throttled_in_request.contains(&ctx.id);
            let reserved = match self.reserve_credential(ctx.id, None).await {
                Err(_) if !can_switch => {
                    let deadline =
                        *queue_deadline.get_or_insert_with(|| Instant::now() + LOCAL_LIMIT_QUEUE);
                    self.reserve_credential(ctx.id, Some(deadline)).await
                }
                reserved => reserved,
            };
            // MCP 响应很短，名额只占到本次尝试结束
            let _credential_in_flight = match reserved {
                Ok(guard) => guard,
                Err(full) => {
                    throttled = true;
                    tracing::info!("[LOCAL-LIMIT] credential={} {}（MCP）", ctx.id, full);
                    self.token_manager.report_throttled_for_rotation(ctx.id);
                    if let Some(cid) = continuation_id.as_deref() {
                        self.token_manager.report_sticky_throttled(cid, ctx.id);
                    }
                    push_unique(&mut throttled_in_request, ctx.id);
                    last_error = Some(anyhow::anyhow!("账号 {} 本地限额：{}", ctx.id, full));
                    if can_switch {
                        continue;
                    }
                    break;
                }
            };

            let _global_permit = self.concurrency_limit.acquire().await?;
            let _cred_permit = self.semaphore_for(ctx.id).acquire_owned().await?;

            let url = self.mcp_url_for(&ctx.credentials);
            let headers = match self.build_mcp_headers(&ctx, attempt) {
                Ok(h) => h,
                Err(e) => {
                    last_error = Some(e);
                    continue;
                }
            };

            // 发送请求（MCP 调用不涉及 /compact 压缩语义，固定使用普通超时）
            let client = match self.client_for(&ctx.credentials, false) {
                Ok(c) => c,
                Err(e) => {
                    last_error = Some(e);
                    continue;
                }
            };
            let effective_mcp_body = Self::rewrite_profile_arn(request_body, &ctx.credentials);
            let response = match client
                .post(&url)
                .headers(headers)
                .body(effective_mcp_body)
                .send()
                .await
            {
                Ok(resp) => resp,
                Err(e) => {
                    let e = anyhow::Error::from(e);
                    tracing::warn!(
                        "MCP 请求发送失败（尝试 {}/{}）: {:#}",
                        attempt + 1,
                        max_retries,
                        e
                    );
                    self.token_manager
                        .report_unreachable(ctx.id, AccountError::new(None, &format!("{e:#}")));
                    push_unique(&mut throttled_in_request, ctx.id);
                    last_error = Some(e);
                    backoff = (attempt + 1 < max_retries).then(|| Self::retry_delay(attempt));
                    continue;
                }
            };

            let status = response.status();

            // 成功响应
            if status.is_success() {
                self.token_manager.report_success(ctx.id);
                // MCP 响应很短，拿到 200 即视为正常完成
                self.token_manager.report_response_completed(ctx.id, None);
                if let Some(cid) = continuation_id.as_deref() {
                    self.token_manager.report_sticky_success(cid, ctx.id);
                }
                return Ok((response, ctx.id));
            }

            // 失败响应
            let body = response.text().await.unwrap_or_default();

            // 402 额度用尽
            if status.as_u16() == 402 && Self::is_monthly_request_limit(&body) {
                let has_available = self.token_manager.report_quota_exhausted(ctx.id);
                if !has_available {
                    // 全局无可用账号：必须走 describe_unavailable 产出带
                    // QUOTA_EXHAUSTED_ALL_MARKER 的文案，否则这条错误串在
                    // handlers.rs 会被误判进 502 兜底，而非正确的 402。
                    anyhow::bail!(
                        "{}",
                        self.token_manager.describe_unavailable(None, bound_ids)
                    );
                }
                last_error = Some(anyhow::anyhow!("MCP 请求失败: {} {}", status, body));
                continue;
            }

            // 400 Bad Request - 请求问题，重试/切换账号无意义；
            // 例外：企业 IdC 账号缺失 profileArn 属确定性配置缺陷——首即禁用
            // （ProfileArnMissing，不做失败计数）并故障转移到其他账号
            if status.as_u16() == 400 {
                if Self::is_profile_arn_required_error(&body) {
                    tracing::warn!(
                        "MCP 请求失败（账号缺少 profileArn，尝试 {}/{}）: {} {}",
                        attempt + 1,
                        max_retries,
                        status,
                        body
                    );
                    // 确定性配置缺陷：首即禁用（ProfileArnMissing），不做失败计数
                    let has_available = self.token_manager.report_profile_arn_missing(ctx.id);
                    if let Some(ref store) = self.failure_log_store {
                        store.record(ctx.id, "mcp", status.as_u16(), &body);
                    }
                    if !has_available {
                        anyhow::bail!("MCP 请求失败（所有账号已用尽）: {} {}", status, body);
                    }
                    last_error = Some(anyhow::anyhow!("MCP 请求失败: {} {}", status, body));
                    continue;
                }
                anyhow::bail!("MCP 请求失败: {} {}", status, body);
            }

            // 401/403 账号问题
            if matches!(status.as_u16(), 401 | 403) {
                let has_available = self
                    .token_manager
                    .report_failure(ctx.id, AccountError::new(Some(status.as_u16()), &body));
                push_unique(&mut throttled_in_request, ctx.id);
                if let Some(ref store) = self.failure_log_store {
                    store.record(ctx.id, "mcp", status.as_u16(), &body);
                }
                if !has_available {
                    anyhow::bail!("MCP 请求失败（所有账号已用尽）: {} {}", status, body);
                }
                last_error = Some(anyhow::anyhow!("MCP 请求失败: {} {}", status, body));
                continue;
            }

            // 429 Too Many Requests - 限流：驱逐 sticky cache + rotation bias 轮转
            if status.as_u16() == 429 {
                tracing::warn!(
                    "MCP 请求失败（上游限流，{}重试，尝试 {}/{}）: {} {}",
                    if small_pool {
                        "单账号延长间隔"
                    } else {
                        "切换账号"
                    },
                    attempt + 1,
                    max_retries,
                    status,
                    body
                );
                self.token_manager.report_throttled(ctx.id);
                self.token_manager.report_throttled_for_rotation(ctx.id);
                if let Some(cid) = continuation_id.as_deref() {
                    self.token_manager.report_sticky_throttled(cid, ctx.id);
                }
                push_unique(&mut throttled_in_request, ctx.id);
                push_unique(&mut upstream_throttled, ctx.id);
                throttled = true;
                if let Some(ref store) = self.throttle_log_store {
                    let context = crate::model::throttle_log::ThrottleContext {
                        credential_rpm: self.rpm_tracker.as_ref().map(|r| r.credential_rpm(ctx.id)),
                        credential_in_flight: self
                            .rpm_tracker
                            .as_ref()
                            .map(|r| r.credential_in_flight(ctx.id)),
                        attempt: Some(attempt as u32 + 1),
                        model: None,
                    };
                    store.record(ctx.id, "mcp", status.as_u16(), &body, None, context);
                }
                last_error = Some(anyhow::anyhow!("MCP 请求失败: {} {}", status, body));
                continue;
            }

            // 408/5xx - 瞬态上游错误
            if status.as_u16() == 408 || status.is_server_error() {
                tracing::warn!(
                    "MCP 请求失败（上游瞬态错误，尝试 {}/{}）: {} {}",
                    attempt + 1,
                    max_retries,
                    status,
                    body
                );
                last_error = Some(anyhow::anyhow!("MCP 请求失败: {} {}", status, body));
                backoff = (attempt + 1 < max_retries).then(|| Self::retry_delay(attempt));
                continue;
            }

            // 其他 4xx
            if status.is_client_error() {
                anyhow::bail!("MCP 请求失败: {} {}", status, body);
            }

            // 兜底
            last_error = Some(anyhow::anyhow!("MCP 请求失败: {} {}", status, body));
            backoff = (attempt + 1 < max_retries).then(|| Self::retry_delay(attempt));
        }

        let last_error = last_error.unwrap_or_else(|| {
            anyhow::anyhow!("MCP 请求失败：已达到最大重试次数（{}次）", max_retries)
        });
        if throttled {
            return Err(anyhow::anyhow!(
                "MCP 请求失败（限流）: 429 Too Many Requests；最后一次错误: {last_error}"
            ));
        }
        Err(last_error)
    }
}
