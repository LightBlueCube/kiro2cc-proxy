// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 上游异常终止判定
//!
//! 以下情形上游流仍可能以正常 EOF 结束。若照常按 end_turn 收尾，客户端会把
//! 截断/丢弃的响应当成任务完成（表现为工具调用被吞、对话停住且不重试）：
//! - 流内 error / exception 帧、invalidStateEvent
//! - metadataEvent.stopReason 为 MALFORMED_*，或为 TOOL_USE 却未收到任何 toolUseEvent
//! - toolUseEvent 帧解析失败（工具调用整帧丢失）
//! - 解码器因连续错误停止、缓冲区溢出，或 EOF 时残留半帧
//!
//! 流结束时 `failure()` 返回 Some 即改发可重试错误，而非正常收尾。
//!
//! 另外，metadataEvent.stopReason 为 CONTENT_FILTERED 表示模型拒答（流里往往没有任何内容），
//! `refusal()` 给出上游的说明原文，流结束时原样返回给客户端，不按空响应处理。

use crate::kiro::model::events::{Event, UpstreamStop};
use crate::kiro::parser::error::ParseError;

/// 输出超长异常：上游的 max_tokens 语义，不属于异常终止
pub(crate) const CONTENT_LENGTH_EXCEEDED_EXCEPTION: &str = "ContentLengthExceededException";

/// 流中途限流：响应头已是 200，限流只能以流内 exception / error 帧下发
const THROTTLING_EXCEPTION: &str = "ThrottlingException";

#[derive(Debug, Default)]
pub(crate) struct UpstreamTermination {
    /// 首个致命信号（日志与错误文案用）
    fatal: Option<String>,
    /// 收到限流帧时的帧内文案（未收到为 None），供回报上游限流
    throttle_message: Option<String>,
    /// 最后一次带 stopReason 的 metadataEvent 是否为 TOOL_USE
    claimed_tool_use: bool,
    /// 是否收到过 toolUseEvent
    saw_tool_use: bool,
    /// 上游拒答（stopReason=content_filtered）时的说明原文
    refusal: Option<String>,
}

impl UpstreamTermination {
    /// 观察每个成功解析的上游事件
    pub(crate) fn observe_event(&mut self, event: &Event) {
        match event {
            Event::ToolUse(_) => self.saw_tool_use = true,
            Event::Metadata(metadata) => {
                let Some(stop) = metadata.upstream_stop() else {
                    return;
                };
                let raw = metadata.stop_reason.as_deref().unwrap_or_default();
                tracing::debug!(stop_reason = raw, "[metadata] 上游 stopReason");
                self.claimed_tool_use = stop == UpstreamStop::ToolUse;
                if stop == UpstreamStop::Malformed {
                    self.mark_fatal(format!("stopReason={raw}"));
                }
                if stop == UpstreamStop::ContentFiltered {
                    let details = metadata.refusal_details();
                    tracing::warn!(
                        category = ?details.and_then(|d| d.category.as_deref()),
                        recommended_model = ?details.and_then(|d| d.recommended_model.as_deref()),
                        explanation = ?details.and_then(|d| d.explanation.as_deref()),
                        "上游拒答"
                    );
                    self.refusal = Some(metadata.refusal_message());
                }
            }
            Event::Error {
                error_code,
                error_message,
            } => {
                self.note_throttle(error_code, error_message);
                self.mark_fatal(error_code.clone())
            }
            Event::Exception {
                exception_type,
                message,
            } if exception_type != CONTENT_LENGTH_EXCEEDED_EXCEPTION => {
                self.note_throttle(exception_type, message);
                self.mark_fatal(exception_type.clone())
            }
            _ => {}
        }
    }

    /// 流内收到过限流帧时返回帧内文案：该账号应按 429 记账避让
    pub(crate) fn throttle_message(&self) -> Option<&str> {
        self.throttle_message.as_deref()
    }

    fn note_throttle(&mut self, kind: &str, message: &str) {
        if kind == THROTTLING_EXCEPTION && self.throttle_message.is_none() {
            self.throttle_message = Some(format!("{kind}: {message}"));
        }
    }

    /// 事件帧解析失败：toolUseEvent 丢失即工具调用被吞，按致命处理；其余仅记录
    pub(crate) fn observe_parse_failure(&mut self, tool_use_frame: bool, err: &ParseError) {
        tracing::warn!(tool_use_frame, "事件帧解析失败，帧内容被丢弃: {}", err);
        if tool_use_frame {
            self.mark_fatal("malformed toolUseEvent");
        }
    }

    /// 解码错误：解码器停止或缓冲区溢出意味着后续帧全部丢失
    pub(crate) fn observe_decode_error(&mut self, err: &ParseError) {
        if matches!(
            err,
            ParseError::TooManyErrors { .. } | ParseError::BufferOverflow { .. }
        ) {
            self.mark_fatal("event stream decoding failed");
        }
    }

    /// 上游 EOF：解码缓冲区残留字节说明流在帧中间被截断
    pub(crate) fn observe_eof(&mut self, residual_bytes: usize) {
        if residual_bytes > 0 {
            tracing::warn!(residual_bytes, "上游流结束时残留未完成的帧");
            self.mark_fatal("stream ended mid-frame");
        }
    }

    /// 上游拒答说明原文：流结束时原样返回给客户端，优先于 [`Self::failure`] 与空响应判定
    pub(crate) fn refusal(&self) -> Option<&str> {
        self.refusal.as_deref()
    }

    /// 异常终止原因；None 表示可以正常收尾
    pub(crate) fn failure(&self) -> Option<String> {
        if let Some(reason) = &self.fatal {
            return Some(reason.clone());
        }
        let tool_use_lost = self.claimed_tool_use && !self.saw_tool_use;
        tool_use_lost.then(|| "stopReason=TOOL_USE without toolUseEvent".to_string())
    }

    fn mark_fatal(&mut self, reason: impl Into<String>) {
        if self.fatal.is_none() {
            self.fatal = Some(reason.into());
        }
    }
}

/// 异常终止时返回给客户端的错误文案（流式 error 事件与非流式响应共用）
pub(crate) fn upstream_aborted_message(reason: &str) -> String {
    format!("Upstream aborted the response before it finished ({reason}). Please retry.")
}

/// 上游在 thinking 之后无 stopReason 地结束、没有正文也没有工具调用时的错误文案
/// （流式 error 事件与非流式响应共用）
pub(crate) const THINKING_ONLY_TRUNCATED_MESSAGE: &str =
    "Upstream stopped after the thinking block without producing a reply. Please retry.";

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kiro::model::events::{MetadataEvent, ToolUseEvent};

    fn metadata(stop_reason: &str) -> Event {
        Event::Metadata(MetadataEvent {
            stop_reason: Some(stop_reason.to_string()),
            ..Default::default()
        })
    }

    fn tool_use() -> Event {
        Event::ToolUse(ToolUseEvent {
            name: "Read".to_string(),
            tool_use_id: "toolu_1".to_string(),
            input: "{}".to_string(),
            stop: true,
        })
    }

    #[test]
    fn test_normal_completion_is_not_failure() {
        let mut t = UpstreamTermination::default();
        t.observe_event(&tool_use());
        t.observe_event(&metadata("TOOL_USE"));
        t.observe_eof(0);
        assert_eq!(t.failure(), None);

        let mut t = UpstreamTermination::default();
        t.observe_event(&metadata("END_TURN"));
        t.observe_event(&metadata("MAX_TOKENS"));
        assert_eq!(t.failure(), None);
    }

    #[test]
    fn test_tool_use_claimed_without_event_is_failure() {
        let mut t = UpstreamTermination::default();
        t.observe_event(&metadata("TOOL_USE"));
        assert!(t.failure().unwrap().contains("TOOL_USE"));
    }

    #[test]
    fn test_malformed_and_error_frames_are_failures() {
        let mut t = UpstreamTermination::default();
        t.observe_event(&metadata("MALFORMED_TOOL_USE"));
        assert_eq!(
            t.failure().as_deref(),
            Some("stopReason=MALFORMED_TOOL_USE")
        );
        // 致命信号只记首个
        t.observe_event(&Event::Error {
            error_code: "InternalServerError".to_string(),
            error_message: String::new(),
        });
        assert_eq!(
            t.failure().as_deref(),
            Some("stopReason=MALFORMED_TOOL_USE")
        );

        let mut t = UpstreamTermination::default();
        t.observe_event(&Event::Exception {
            exception_type: "ThrottlingException".to_string(),
            message: "slow down".to_string(),
        });
        assert_eq!(t.failure().as_deref(), Some("ThrottlingException"));
        assert_eq!(t.throttle_message(), Some("ThrottlingException: slow down"));
    }

    #[test]
    fn test_throttle_detected_in_error_frame_only_for_throttling() {
        let mut t = UpstreamTermination::default();
        t.observe_event(&Event::Error {
            error_code: "InternalServerError".to_string(),
            error_message: String::new(),
        });
        assert_eq!(t.throttle_message(), None);

        // 限流晚于其它致命信号到达也要记下：failure 只记首个，限流回报与之独立
        t.observe_event(&Event::Error {
            error_code: "ThrottlingException".to_string(),
            error_message: "rate".to_string(),
        });
        assert_eq!(t.failure().as_deref(), Some("InternalServerError"));
        assert_eq!(t.throttle_message(), Some("ThrottlingException: rate"));
    }

    #[test]
    fn test_content_filtered_records_refusal_explanation() {
        let mut t = UpstreamTermination::default();
        t.observe_event(&metadata("END_TURN"));
        assert_eq!(t.refusal(), None);

        let event = Event::Metadata(
            serde_json::from_str(
                r#"{"stopReason":"content_filtered","stopDetails":{"refusal":{"explanation":"restricted"}}}"#,
            )
            .unwrap(),
        );
        t.observe_event(&event);
        assert_eq!(t.refusal(), Some("restricted"));
        assert_eq!(t.failure(), None, "拒答单独返回，不算异常终止");
    }

    #[test]
    fn test_content_length_exception_is_not_failure() {
        let mut t = UpstreamTermination::default();
        t.observe_event(&Event::Exception {
            exception_type: CONTENT_LENGTH_EXCEEDED_EXCEPTION.to_string(),
            message: String::new(),
        });
        assert_eq!(t.failure(), None);
    }

    #[test]
    fn test_decode_and_parse_failures() {
        let parse_err = || ParseError::InvalidMessageType("x".to_string());

        let mut t = UpstreamTermination::default();
        t.observe_parse_failure(false, &parse_err());
        t.observe_decode_error(&ParseError::MessageCrcMismatch {
            expected: 0,
            actual: 1,
        });
        assert_eq!(t.failure(), None, "可恢复的单帧错误不判为异常终止");

        t.observe_parse_failure(true, &parse_err());
        assert_eq!(t.failure().as_deref(), Some("malformed toolUseEvent"));

        let mut t = UpstreamTermination::default();
        t.observe_decode_error(&ParseError::TooManyErrors {
            count: 5,
            last_error: String::new(),
        });
        assert!(t.failure().is_some());

        let mut t = UpstreamTermination::default();
        t.observe_eof(7);
        assert_eq!(t.failure().as_deref(), Some("stream ended mid-frame"));
    }
}
