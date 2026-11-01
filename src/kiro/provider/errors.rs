//! 错误分类与退避：重试/限流退避延迟、月度限额与 Profile ARN 错误判定、请求体改写。
use std::time::Duration;

use crate::kiro::model::credentials::{KiroCredentials, fallback_profile_arn_value};

use crate::model::rpm::{InFlightGuard, LimitFull, LocalLimits};

use super::core::KiroProvider;

const THROTTLE_BASE_MS: u64 = 2000;
const THROTTLE_STEP_MS: u64 = 1000;
const THROTTLE_MAX_MS: u64 = 8_000;
const THROTTLE_JITTER_MAX_MS: u64 = 1500;
const RETRY_BACKOFF_MAX_EXPONENT: usize = 6;
/// 每个请求为等待账号本地限额（RPM / 并发）名额最多排队的总时长
pub(crate) const LOCAL_LIMIT_QUEUE: Duration = Duration::from_secs(10);
const THINKING_SIGNATURE_INVALID_REASON: &str = "THINKING_SIGNATURE_INVALID";

impl KiroProvider {
    pub(crate) fn retry_delay(attempt: usize) -> Duration {
        // 指数退避 + 少量抖动，避免上游抖动时放大故障
        const BASE_MS: u64 = 200;
        const MAX_MS: u64 = 5_000;
        let exp = BASE_MS
            .saturating_mul(2u64.saturating_pow(attempt.min(RETRY_BACKOFF_MAX_EXPONENT) as u32));
        let backoff = exp.min(MAX_MS);
        let jitter_max = (backoff / 4).max(1);
        let jitter = fastrand::u64(0..=jitter_max);
        Duration::from_millis(backoff.saturating_add(jitter))
    }

    /// 429 限流退避：随 attempt 递增，避免固定间隔反复命中同一限流窗口
    pub(crate) fn throttle_delay(attempt: usize) -> Duration {
        // 2s + attempt×1s（上限 8s）+ jitter
        let base =
            THROTTLE_BASE_MS.saturating_add((attempt as u64).saturating_mul(THROTTLE_STEP_MS));
        let capped = base.min(THROTTLE_MAX_MS);
        let jitter = fastrand::u64(0..=THROTTLE_JITTER_MAX_MS);
        Duration::from_millis(capped.saturating_add(jitter))
    }

    /// 本地 RPM 已达软上限的账号（软上限为 0 时关闭，返回空）
    pub(crate) fn soft_rpm_full_credentials(&self) -> Vec<u64> {
        let soft = u64::from(self.token_manager.soft_rpm_per_credential());
        let Some(rpm) = self.rpm_tracker.as_ref().filter(|_| soft > 0) else {
            return Vec::new();
        };
        self.token_manager
            .credential_ids()
            .into_iter()
            .filter(|&id| rpm.credential_rpm(id) >= soft)
            .collect()
    }

    /// 向账号发出一次请求前占用其本地限额名额（RPM 计 1 次、在途 +1，守卫 drop 时在途 -1）
    ///
    /// `queue_until` 为 None 时满了立即返回（还有别的账号可换）；为 Some 时排队到该时刻。
    /// 未启用 RPM 监控时不设限，返回 `Ok(None)`。
    pub(crate) async fn reserve_credential(
        &self,
        credential_id: u64,
        queue_until: Option<tokio::time::Instant>,
    ) -> Result<Option<InFlightGuard>, LimitFull> {
        let Some(rpm) = &self.rpm_tracker else {
            return Ok(None);
        };
        let limits = LocalLimits {
            rpm: self.token_manager.max_rpm_per_credential(),
            concurrency: self.token_manager.max_concurrency_per_credential(),
        };
        let guard = match queue_until {
            None => rpm.try_begin_credential_request(credential_id, limits)?,
            Some(deadline) => {
                rpm.begin_credential_request_by(credential_id, limits, deadline)
                    .await?
            }
        };
        Ok(Some(guard))
    }

    pub(crate) fn is_monthly_request_limit(body: &str) -> bool {
        if body.contains("MONTHLY_REQUEST_COUNT") {
            return true;
        }

        let Ok(value) = serde_json::from_str::<serde_json::Value>(body) else {
            return false;
        };

        if value
            .get("reason")
            .and_then(|v| v.as_str())
            .is_some_and(|v| v == "MONTHLY_REQUEST_COUNT")
        {
            return true;
        }

        value
            .pointer("/error/reason")
            .and_then(|v| v.as_str())
            .is_some_and(|v| v == "MONTHLY_REQUEST_COUNT")
    }

    /// 检测数据面 400 是否为"账号缺少 profileArn"类错误。
    ///
    /// 企业 IdC 账号必须携带 profileArn，缺失时上游返回
    /// `400 {"message":"profileArn is required for this request."}`，
    /// 属账号级缺陷而非请求级错误，应故障转移到其他账号。
    pub(crate) fn is_profile_arn_required_error(body: &str) -> bool {
        body.contains("profileArn is required")
    }

    /// 检测数据面 400 是否为"历史推理签名无效"。
    ///
    /// 回传的 `reasoningContent` 签名损坏（截断、改动）或来自其他服务时，上游返回
    /// `400 {"message":"messages.3.content.0: Invalid `signature` in `thinking` block",
    /// "reason":"THINKING_SIGNATURE_INVALID"}`。跨账号、跨模型的真实签名不会被拒（2026-10-30 实测）。
    pub(crate) fn is_thinking_signature_invalid(body: &str) -> bool {
        body.contains(THINKING_SIGNATURE_INVALID_REASON)
    }

    /// 签名无效报错点名的 history 下标
    ///
    /// 报错文案形如 `messages.3.content.0: ...`。`messages.N` 即实际发出的请求体的
    /// `conversationState.history[N]`（2026-10-30 实测：多轮、带 system、tool_use/tool_result
    /// 交替、adaptive 无前缀对，点名下标均与 history 下标一致）。
    pub(crate) fn rejected_history_index(body: &str) -> Option<usize> {
        let value = serde_json::from_str::<serde_json::Value>(body).ok()?;
        let message = value.get("message")?.as_str()?;
        message
            .strip_prefix("messages.")?
            .split('.')
            .next()?
            .parse()
            .ok()
    }

    /// 只删除 `history[index]` 的 `reasoningContent`，返回 `(新请求体, 被删的签名)`
    ///
    /// 该位置不是带推理的 assistant 消息（下标映射与预期不符）时返回 None。
    pub(crate) fn strip_history_reasoning_at(body: &str, index: usize) -> Option<(String, String)> {
        let mut value = serde_json::from_str::<serde_json::Value>(body).ok()?;
        let message = value
            .pointer_mut(&format!(
                "/conversationState/history/{index}/assistantResponseMessage"
            ))?
            .as_object_mut()?;
        let reasoning = message.remove("reasoningContent")?;
        let signature = reasoning
            .pointer("/reasoningText/signature")?
            .as_str()?
            .to_string();
        Some((value.to_string(), signature))
    }

    /// 删除 history 中所有 `assistantResponseMessage.reasoningContent`，用于签名被拒后的降级重试。
    ///
    /// 没有可删的字段（或 JSON 无法解析）时返回 None：此时重试结果不会变，调用方应直接报错。
    pub(crate) fn strip_history_reasoning(body: &str) -> Option<String> {
        let mut value = serde_json::from_str::<serde_json::Value>(body).ok()?;
        let history = value
            .pointer_mut("/conversationState/history")?
            .as_array_mut()?;

        let mut stripped = false;
        for entry in history {
            if let Some(message) = entry
                .get_mut("assistantResponseMessage")
                .and_then(|m| m.as_object_mut())
            {
                stripped |= message.remove("reasoningContent").is_some();
            }
        }
        stripped.then(|| value.to_string())
    }

    /// 无 profile_arn 账号的数据面 fallback ARN。
    ///
    /// 常量与推导逻辑统一定义在 [`crate::kiro::model::credentials`]（添加/加载账号时
    /// 即按此补全并持久化），此处委托保持行为一致。
    pub(crate) fn fallback_profile_arn(credentials: &KiroCredentials) -> Option<&'static str> {
        fallback_profile_arn_value(credentials)
    }

    /// 将请求 body 中的 `profileArn` 替换为当前选中账号的值。
    ///
    /// - 账号有 profile_arn → 设置 / 覆盖字段
    /// - 账号无 profile_arn → 按账号类型注入固定 ARN（见 [`Self::fallback_profile_arn`]）：
    ///   上游数据面对所有账号都要求该字段存在，缺失会 400 "profileArn is required"
    /// - JSON 解析失败 → 原样返回，不阻断请求
    pub(crate) fn rewrite_profile_arn(body: &str, credentials: &KiroCredentials) -> String {
        let Ok(mut value) = serde_json::from_str::<serde_json::Value>(body) else {
            return body.to_string();
        };
        let obj = match value.as_object_mut() {
            Some(o) => o,
            None => return body.to_string(),
        };
        let arn = match &credentials.profile_arn {
            Some(arn) => Some(arn.as_str()),
            None => Self::fallback_profile_arn(credentials),
        };
        match arn {
            Some(arn) => {
                obj.insert(
                    "profileArn".to_string(),
                    serde_json::Value::String(arn.to_string()),
                );
            }
            None => {
                obj.remove("profileArn");
            }
        }

        serde_json::to_string(&value).unwrap_or_else(|_| body.to_string())
    }
}
