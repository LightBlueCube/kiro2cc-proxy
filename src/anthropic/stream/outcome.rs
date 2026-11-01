// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 上游响应读完后的结局：流式与非流式共用同一套客户端错误、账号记账与 usage 错误标注口径。
//!
//! ```text
//! 结局                     客户端                          账号            usage 错误标注
//! Completed                正常响应                        completed       —
//! Refused                  403 permission_error（原文）    该模型拒答计数  refused
//! Aborted                  502 overloaded_error            account_fault 时计异常  aborted
//! ThinkingOnlyTruncated    502 overloaded_error            计异常          thinking_only_truncated
//! Empty                    502 overloaded_error            计异常          empty
//! EmptyOversizedContext    400 invalid_request_error       不记            empty_oversized_context
//! ```
//!
//! 出错的请求只在上游计费（收到 meteringEvent）时才写 usage 记录。

use axum::http::StatusCode;

use super::state::SseEvent;
use super::termination::{Abort, UpstreamTermination};
use crate::kiro::provider::UpstreamFeedback;
use crate::model::usage::UsageError;

/// 小上下文空响应：疑似偶发，客户端可重试
const EMPTY_RESPONSE_RETRY_MESSAGE: &str = "Upstream returned an empty response. Please retry.";

/// 大上下文空响应：原样重试还是同样的大请求，提示压缩上下文
const EMPTY_RESPONSE_OVERSIZED_MESSAGE: &str = "Upstream returned an empty response, likely because the context is too large. \
     Reduce conversation history (e.g. /compact), system prompt, or tools, then retry.";

/// 上游在 thinking 之后无 stopReason 地结束、没有正文也没有工具调用
pub(crate) const THINKING_ONLY_TRUNCATED_MESSAGE: &str =
    "Upstream stopped after the thinking block without producing a reply. Please retry.";

/// 拒答用 permission_error（不可重试）：拒答多与内容有关，客户端自动重试会把同一请求轮番发到
/// 其它账号，可能连累它们也被上游限制
const REFUSAL_ERROR_TYPE: &str = "permission_error";

/// 可重试的上游错误
const OVERLOADED_ERROR_TYPE: &str = "overloaded_error";

/// 记为账号最近报错的说明（面板展示）
const EMPTY_RESPONSE_DETAIL: &str = "Empty response";
const THINKING_ONLY_TRUNCATED_DETAIL: &str = "Stopped after thinking without a reply";

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum ResponseOutcome {
    /// 正常完成（含上游给了收尾理由、只有 thinking 的回复）
    Completed,
    /// 模型拒答（stopReason=content_filtered），参数为上游说明原文
    Refused(String),
    Aborted(Abort),
    /// thinking 之后无收尾理由结束，没有正文也没有工具调用
    ThinkingOnlyTruncated,
    /// 200 正常结束但没有任何内容
    Empty,
    /// 上下文过大导致的空响应：换账号也一样，不计入账号
    EmptyOversizedContext,
}

/// 返回给客户端的错误
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct ClientError {
    /// 非流式响应的 HTTP 状态码（流式响应头已是 200，只用 error 事件）
    pub(crate) status: StatusCode,
    pub(crate) error_type: &'static str,
    pub(crate) message: String,
}

impl ResponseOutcome {
    /// 不必组装内容就能判定的结局（拒答优先于异常终止，异常终止优先于空响应）；
    /// None 表示有内容，由调用方组装后再判定是 Completed 还是 ThinkingOnlyTruncated
    pub(crate) fn before_content(
        termination: &UpstreamTermination,
        empty: bool,
        oversized_context: bool,
    ) -> Option<Self> {
        if let Some(message) = termination.refusal() {
            return Some(Self::Refused(message.to_string()));
        }
        if let Some(abort) = termination.failure() {
            return Some(Self::Aborted(abort));
        }
        if !empty {
            return None;
        }
        Some(if oversized_context {
            Self::EmptyOversizedContext
        } else {
            Self::Empty
        })
    }

    pub(crate) fn client_error(&self) -> Option<ClientError> {
        let (status, error_type, message) = match self {
            Self::Completed => return None,
            Self::Refused(message) => (StatusCode::FORBIDDEN, REFUSAL_ERROR_TYPE, message.clone()),
            Self::Aborted(abort) => (
                StatusCode::BAD_GATEWAY,
                OVERLOADED_ERROR_TYPE,
                format!(
                    "Upstream aborted the response before it finished ({}). Please retry.",
                    abort.reason
                ),
            ),
            Self::ThinkingOnlyTruncated => (
                StatusCode::BAD_GATEWAY,
                OVERLOADED_ERROR_TYPE,
                THINKING_ONLY_TRUNCATED_MESSAGE.to_string(),
            ),
            Self::Empty => (
                StatusCode::BAD_GATEWAY,
                OVERLOADED_ERROR_TYPE,
                EMPTY_RESPONSE_RETRY_MESSAGE.to_string(),
            ),
            Self::EmptyOversizedContext => (
                StatusCode::BAD_REQUEST,
                "invalid_request_error",
                EMPTY_RESPONSE_OVERSIZED_MESSAGE.to_string(),
            ),
        };
        Some(ClientError {
            status,
            error_type,
            message,
        })
    }

    /// 流式响应的 error 事件
    pub(crate) fn error_event(&self) -> Option<SseEvent> {
        let error = self.client_error()?;
        Some(SseEvent::new(
            "error",
            serde_json::json!({
                "type": "error",
                "error": { "type": error.error_type, "message": error.message }
            }),
        ))
    }

    /// 回报账号：拒答记到该模型，怪账号的异常结束计入健康度，正常完成清零连续失败
    pub(crate) fn report(&self, feedback: &UpstreamFeedback) {
        match self {
            Self::Completed => feedback.completed(),
            Self::Refused(message) => feedback.refused(message),
            Self::Aborted(abort) if abort.account_fault => {
                feedback.abnormal(&format!("Upstream aborted: {}", abort.reason))
            }
            Self::ThinkingOnlyTruncated => feedback.abnormal(THINKING_ONLY_TRUNCATED_DETAIL),
            Self::Empty => feedback.abnormal(EMPTY_RESPONSE_DETAIL),
            Self::Aborted(_) | Self::EmptyOversizedContext => {}
        }
    }

    pub(crate) fn usage_error(&self) -> Option<UsageError> {
        match self {
            Self::Completed => None,
            Self::Refused(_) => Some(UsageError::Refused),
            Self::Aborted(_) => Some(UsageError::Aborted),
            Self::ThinkingOnlyTruncated => Some(UsageError::ThinkingOnlyTruncated),
            Self::Empty => Some(UsageError::Empty),
            Self::EmptyOversizedContext => Some(UsageError::EmptyOversizedContext),
        }
    }
}

/// 出错的请求只在上游计费（收到 meteringEvent）时才写 usage 记录
pub(crate) fn should_record_usage(error: Option<UsageError>, credits_used: Option<f64>) -> bool {
    error.is_none() || credits_used.is_some()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kiro::model::events::{Event, MetadataEvent};

    fn termination_with(event: Event) -> UpstreamTermination {
        let mut t = UpstreamTermination::default();
        t.observe_event(&event);
        t
    }

    #[test]
    fn refusal_wins_over_abort_and_empty() {
        let mut t = termination_with(Event::Error {
            error_code: "InternalServerError".to_string(),
            error_message: String::new(),
        });
        t.observe_event(&Event::Metadata(
            serde_json::from_str(
                r#"{"stopReason":"content_filtered","stopDetails":{"refusal":{"explanation":"no"}}}"#,
            )
            .unwrap(),
        ));
        assert_eq!(
            ResponseOutcome::before_content(&t, true, false),
            Some(ResponseOutcome::Refused("no".to_string()))
        );
    }

    #[test]
    fn empty_split_by_context_size_and_content_continues() {
        let t = UpstreamTermination::default();
        assert_eq!(
            ResponseOutcome::before_content(&t, true, false),
            Some(ResponseOutcome::Empty)
        );
        assert_eq!(
            ResponseOutcome::before_content(&t, true, true),
            Some(ResponseOutcome::EmptyOversizedContext)
        );
        assert_eq!(ResponseOutcome::before_content(&t, false, true), None);
    }

    #[test]
    fn client_errors_by_outcome() {
        let oversized = ResponseOutcome::EmptyOversizedContext
            .client_error()
            .unwrap();
        assert_eq!(oversized.status, StatusCode::BAD_REQUEST);
        assert_eq!(oversized.error_type, "invalid_request_error");
        assert!(oversized.message.contains("context is too large"));

        let refused = ResponseOutcome::Refused("restricted".to_string());
        let event = refused.error_event().unwrap();
        assert_eq!(event.event, "error");
        assert_eq!(event.data["error"]["type"], "permission_error");
        assert_eq!(event.data["error"]["message"], "restricted");

        let t = termination_with(Event::Metadata(MetadataEvent {
            stop_reason: Some("MALFORMED_TOOL_USE".to_string()),
            ..Default::default()
        }));
        let aborted = ResponseOutcome::before_content(&t, false, false).unwrap();
        let error = aborted.client_error().unwrap();
        assert_eq!(error.status, StatusCode::BAD_GATEWAY);
        assert_eq!(error.error_type, "overloaded_error");
        assert!(error.message.contains("MALFORMED_TOOL_USE") && error.message.contains("retry"));

        assert_eq!(ResponseOutcome::Completed.client_error(), None);
    }

    #[test]
    fn failed_usage_recorded_only_when_billed() {
        assert!(should_record_usage(None, None));
        assert!(!should_record_usage(Some(UsageError::Empty), None));
        assert!(should_record_usage(Some(UsageError::Empty), Some(0.4)));
    }
}
