// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! CreateResponse SSE 响应 → Kiro 事件
//!
//! 与 Kiro 客户端（KAS 3.1.0 `parseCreateResponseSse`）同构：把 Responses 流事件还原成
//! GenerateAssistantResponse 的事件（[`Event`]），下游状态机无需区分上游 API。
//!
//! ```text
//! response.output_text.delta / refusal.delta     → assistantResponseEvent
//! response.reasoning_text.delta                  → reasoningContentEvent{text}
//! output_item.done{reasoning}                    → reasoningContentEvent{signature}
//! output_item.added{function_call}               → toolUseEvent（开）
//! function_call_arguments.delta                  → toolUseEvent（参数增量）
//! function_call_arguments.done                   → toolUseEvent{stop}
//! response.completed / response.incomplete       → 补齐未收尾的 toolUseEvent{stop}、
//!                                                  metadataEvent{stopReason}、
//!                                                  contextUsageEvent、meteringEvent
//! error / response.failed                        → 错误帧（限流为 ThrottlingException）
//! kiro:code_reference.added                      → codeReferenceEvent
//! ```

use serde_json::Value;

use crate::kiro::decoder::DecodeError;
use crate::kiro::model::events::{
    AssistantResponseEvent, ContextUsageEvent, Event, MetadataEvent, MeteringEvent,
    ReasoningContentEvent, ToolUseEvent,
};
use crate::kiro::parser::error::ParseError;

/// 单行上限（与 KAS 一致）；超出视为流损坏
const MAX_LINE_BYTES: usize = 4 * 1024 * 1024;

/// Kiro 的保活事件，仅用于客户端卡顿检测
const KEEPALIVE_EVENT: &str = "kiro:keep_alive";

/// 流中途限流的异常类型（与 GenerateAssistantResponse 流内异常帧一致）
const THROTTLING_EXCEPTION: &str = "ThrottlingException";

/// 未声明 type、只给出 code 时，这些 code 实为错误类别（KAS `classifyCreateResponseStreamError`）
const ERROR_TYPES_SENT_AS_CODE: [&str; 6] = [
    "invalid_request",
    "not_found",
    "too_many_requests",
    "insufficient_quota",
    "server_error",
    "model_error",
];

type Decoded = Result<Event, DecodeError>;

/// CreateResponse SSE 增量解码器
#[derive(Default)]
pub struct SseEventDecoder {
    /// 尚未遇到换行的半行
    pending_line: Vec<u8>,
    tool_calls: ToolCalls,
    /// 拒答文本已按增量下发，refusal.done 不再重复
    refusal_streamed: bool,
    /// 收到过终止事件（completed / incomplete / failed / error）
    terminated: bool,
}

impl SseEventDecoder {
    pub fn new() -> Self {
        Self::default()
    }

    /// 喂入一段响应体，返回其中完整行解出的事件
    pub fn feed(&mut self, chunk: &[u8]) -> Vec<Decoded> {
        self.pending_line.extend_from_slice(chunk);

        let mut out = Vec::new();
        while let Some(newline) = self.pending_line.iter().position(|&b| b == b'\n') {
            let line: Vec<u8> = self.pending_line.drain(..=newline).collect();
            self.handle_line(&line, &mut out);
        }

        if self.pending_line.len() > MAX_LINE_BYTES {
            out.push(Err(DecodeError::Stream(ParseError::BufferOverflow {
                size: self.pending_line.len(),
                max: MAX_LINE_BYTES,
            })));
            self.pending_line.clear();
        }
        out
    }

    /// 响应体结束：处理末尾不带换行的最后一行
    pub fn finish(&mut self) -> Vec<Decoded> {
        let line = std::mem::take(&mut self.pending_line);
        let mut out = Vec::new();
        self.handle_line(&line, &mut out);
        out
    }

    /// 流在终止事件之前结束（连接被截断）
    pub fn truncated(&self) -> bool {
        !self.terminated
    }

    fn handle_line(&mut self, line: &[u8], out: &mut Vec<Decoded>) {
        let line = String::from_utf8_lossy(line);
        let Some(data) = line.trim_end().strip_prefix("data:") else {
            return;
        };
        let data = data.trim();
        if data.is_empty() || data == "[DONE]" {
            return;
        }

        match serde_json::from_str::<Value>(data) {
            Ok(payload) => self.handle_payload(&payload, out),
            Err(e) => out.push(Err(DecodeError::Event {
                tool_use: data.contains("function_call"),
                error: e.into(),
            })),
        }
    }

    fn handle_payload(&mut self, payload: &Value, out: &mut Vec<Decoded>) {
        let kind = payload["type"].as_str().unwrap_or_default();
        let delta = payload["delta"].as_str();
        let item = &payload["item"];
        let item_kind = item["type"].as_str().unwrap_or_default();

        match kind {
            KEEPALIVE_EVENT => {}
            "response.output_text.delta" => push_text(out, delta),
            "response.reasoning_text.delta" | "response.reasoning.delta" => {
                if let Some(text) = delta {
                    out.push(Ok(Event::ReasoningContent(ReasoningContentEvent {
                        text: text.to_string(),
                        signature: String::new(),
                    })));
                }
            }
            "response.refusal.delta" => {
                self.refusal_streamed = true;
                push_text(out, delta);
            }
            "response.refusal.done" if !self.refusal_streamed => {
                push_text(out, payload["refusal"].as_str());
            }
            "response.output_item.added" if item_kind == "function_call" => {
                out.push(Ok(self.tool_calls.open(item)));
            }
            "response.output_item.done" if item_kind == "function_call" => {
                out.extend(self.tool_calls.item_done(item).map(Ok));
            }
            "response.output_item.done" if item_kind == "reasoning" => {
                out.extend(reasoning_signature(item).map(Ok));
            }
            "response.function_call_arguments.delta" => {
                if let Some(arguments) = delta {
                    out.push(self.tool_calls.arguments_delta(arguments));
                }
            }
            "response.function_call_arguments.done" => {
                out.extend(self.tool_calls.arguments_done().map(Ok));
            }
            "response.completed" | "response.incomplete" => {
                self.terminate(kind, &payload["response"], out);
            }
            "error" => self.fail(&payload["error"], out),
            "response.failed" => self.fail(&payload["response"]["error"], out),
            "kiro:code_reference.added" => {
                out.extend(code_references(&payload["annotations"]));
            }
            _ => {}
        }
    }

    /// 正常或不完整结束：补齐工具调用收尾，再给出 stopReason、上下文占用与 credits
    fn terminate(&mut self, kind: &str, response: &Value, out: &mut Vec<Decoded>) {
        self.terminated = true;

        let incomplete =
            kind == "response.incomplete" || response["status"].as_str() == Some("incomplete");
        let incomplete_reason = response["incomplete_details"]["reason"].as_str();
        if incomplete && self.tool_calls.has_unfinished() {
            tracing::debug!(
                reason = incomplete_reason,
                "CreateResponse 不完整结束，未收尾的工具调用不补 stop"
            );
        }
        out.extend(self.tool_calls.finish_turn(incomplete).into_iter().map(Ok));

        let stop_reason = if incomplete {
            stop_reason_for_incomplete(incomplete_reason)
        } else {
            "END_TURN"
        };
        out.push(Ok(Event::Metadata(MetadataEvent {
            stop_reason: Some(stop_reason.to_string()),
            ..Default::default()
        })));

        let usage = &response["usage"];
        if let Some(percentage) = finite(&usage["kiro:context_usage_percentage"]) {
            out.push(Ok(Event::ContextUsage(ContextUsageEvent {
                context_usage_percentage: percentage,
            })));
        }

        let credits = &usage["kiro:credits"];
        if let Some(value) = finite(&credits["value"]) {
            out.push(Ok(Event::Metering(MeteringEvent {
                unit: credits["unit"].as_str().unwrap_or_default().to_string(),
                unit_plural: credits["unit_plural"]
                    .as_str()
                    .unwrap_or_default()
                    .to_string(),
                usage: value,
                cache_read_input_tokens: None,
                cache_creation_input_tokens: None,
            })));
        }
    }

    /// 流内错误：限流按 ThrottlingException 异常帧下发，其余按错误帧
    fn fail(&mut self, error: &Value, out: &mut Vec<Decoded>) {
        self.terminated = true;
        self.tool_calls = ToolCalls::default();

        let mut error_type = error["type"].as_str();
        let mut code = error["code"].as_str();
        let code_is_type =
            error_type.is_none() && code.is_some_and(|c| ERROR_TYPES_SENT_AS_CODE.contains(&c));
        if code_is_type {
            error_type = code.take();
        }

        let message = error["message"]
            .as_str()
            .filter(|m| !m.is_empty())
            .map(str::to_string)
            .unwrap_or_else(|| {
                let what = error_type.or(code).unwrap_or("unknown");
                format!("CreateResponse stream failed ({what})")
            });

        let throttled = error_type == Some("too_many_requests")
            || code == Some("rate_limit_exceeded")
            || error_type.is_some_and(|t| t.to_ascii_lowercase().contains("throttl"));
        if throttled {
            out.push(Ok(Event::Exception {
                exception_type: THROTTLING_EXCEPTION.to_string(),
                message,
            }));
            return;
        }

        out.push(Ok(Event::Error {
            error_code: code.or(error_type).unwrap_or("unknown").to_string(),
            error_message: message,
        }));
    }
}

/// 进行中的工具调用（KAS 的 openCalls / heldStops 状态）
///
/// Responses 的参数增量不带调用 ID 与工具名；GenerateAssistantResponse 的每个 toolUseEvent
/// 都带，下游依赖这两项聚合参数，故在此补全。
#[derive(Default)]
struct ToolCalls {
    /// 已开始、尚未收到 stop 的调用（保持开始顺序）
    open: Vec<OpenCall>,
    /// 参数增量归属的调用
    current: Option<String>,
    /// output_item.done 已完成、stop 延后到 response.completed 的调用
    held_stops: Vec<OpenCall>,
}

struct OpenCall {
    id: String,
    name: String,
    /// 收到过参数增量（未收到时以 output_item.done 的完整参数补发）
    streamed_arguments: bool,
}

impl ToolCalls {
    fn open(&mut self, item: &Value) -> Event {
        let call = OpenCall {
            id: call_id(item),
            name: item["name"].as_str().unwrap_or_default().to_string(),
            streamed_arguments: false,
        };
        let event = tool_use(&call, item["arguments"].as_str().unwrap_or_default(), false);
        self.current = Some(call.id.clone());
        self.open.push(call);
        event
    }

    fn arguments_delta(&mut self, arguments: &str) -> Decoded {
        let current = self.current.as_deref();
        let Some(call) = self
            .open
            .iter_mut()
            .find(|c| Some(c.id.as_str()) == current)
        else {
            return Err(DecodeError::Event {
                tool_use: true,
                error: ParseError::UnexpectedEvent(
                    "function_call_arguments.delta 没有进行中的工具调用".to_string(),
                ),
            });
        };
        call.streamed_arguments = true;
        Ok(tool_use(call, arguments, false))
    }

    fn arguments_done(&mut self) -> Option<Event> {
        let id = self.current.take()?;
        let call = self.remove_open(&id)?;
        Some(tool_use(&call, "", true))
    }

    /// 参数未按增量到达时补发完整参数；stop 延后到 response.completed
    fn item_done(&mut self, item: &Value) -> Option<Event> {
        let call = self.remove_open(&call_id(item))?;
        let arguments = (!call.streamed_arguments)
            .then(|| tool_use(&call, item["arguments"].as_str().unwrap_or_default(), false));

        if item["status"].as_str() == Some("incomplete") {
            tracing::debug!(
                tool_use_id = call.id,
                "CreateResponse 工具调用不完整，不补 stop"
            );
        } else {
            self.held_stops.push(call);
        }
        arguments
    }

    fn has_unfinished(&self) -> bool {
        !self.open.is_empty() || !self.held_stops.is_empty()
    }

    /// 回合结束：正常结束时给所有未收尾的调用补 stop；不完整结束时丢弃
    fn finish_turn(&mut self, incomplete: bool) -> Vec<Event> {
        let pending = std::mem::take(self);
        if incomplete {
            return Vec::new();
        }
        pending
            .held_stops
            .iter()
            .chain(&pending.open)
            .map(|call| tool_use(call, "", true))
            .collect()
    }

    fn remove_open(&mut self, id: &str) -> Option<OpenCall> {
        let index = self.open.iter().position(|c| c.id == id)?;
        Some(self.open.remove(index))
    }
}

fn call_id(item: &Value) -> String {
    item["call_id"]
        .as_str()
        .or_else(|| item["id"].as_str())
        .unwrap_or_default()
        .to_string()
}

fn tool_use(call: &OpenCall, input: &str, stop: bool) -> Event {
    Event::ToolUse(ToolUseEvent {
        name: call.name.clone(),
        tool_use_id: call.id.clone(),
        input: input.to_string(),
        stop,
    })
}

fn push_text(out: &mut Vec<Decoded>, text: Option<&str>) {
    if let Some(text) = text {
        out.push(Ok(Event::AssistantResponse(
            AssistantResponseEvent::from_content(text),
        )));
    }
}

/// 推理信封 `{"reasoningText":{"text","signature"}}` 中的签名；推理正文已按增量下发
///
/// `{"redactedContent"}` 信封与 GenerateAssistantResponse 路径一样不支持，丢弃。
fn reasoning_signature(item: &Value) -> Option<Event> {
    let envelope = item["encrypted_content"]
        .as_str()
        .filter(|s| !s.is_empty())?;
    let Ok(envelope) = serde_json::from_str::<Value>(envelope) else {
        tracing::debug!("CreateResponse 推理信封无法解析，已忽略");
        return None;
    };
    let Some(signature) = envelope["reasoningText"]["signature"].as_str() else {
        tracing::debug!("CreateResponse 推理信封无签名（redacted 推理不支持），已忽略");
        return None;
    };
    Some(Event::ReasoningContent(ReasoningContentEvent {
        text: String::new(),
        signature: signature.to_string(),
    }))
}

/// incomplete_details.reason → GenerateAssistantResponse 的 stopReason
fn stop_reason_for_incomplete(reason: Option<&str>) -> &'static str {
    match reason {
        Some("max_output_tokens") => "MAX_TOKENS",
        Some("content_filter") => "CONTENT_FILTERED",
        Some("compaction_required_for_model_handoff") => "COMPACTION_REQUIRED_FOR_MODEL_HANDOFF",
        other => {
            tracing::warn!(reason = ?other, "CreateResponse 未知的不完整结束原因，按 END_TURN 处理");
            "END_TURN"
        }
    }
}

fn finite(value: &Value) -> Option<f64> {
    value.as_f64().filter(|v| v.is_finite())
}

/// annotations 中的代码引用（type 缺省或为 kiro:code_reference，且至少带一项来源信息）
fn code_references(annotations: &Value) -> Option<Decoded> {
    let references: Vec<Value> = annotations
        .as_array()?
        .iter()
        .filter(|a| matches!(a["type"].as_str(), None | Some("kiro:code_reference")))
        .filter_map(|a| {
            let mut reference = serde_json::Map::new();
            for (source, target) in [
                ("license_name", "licenseName"),
                ("repository", "repository"),
                ("url", "url"),
            ] {
                if let Some(value) = a[source].as_str() {
                    reference.insert(target.to_string(), Value::String(value.to_string()));
                }
            }
            (!reference.is_empty()).then_some(Value::Object(reference))
        })
        .collect();
    if references.is_empty() {
        return None;
    }

    let event = serde_json::from_value(serde_json::json!({ "references": references }))
        .map(Event::CodeReference)
        .map_err(|e| DecodeError::Event {
            tool_use: false,
            error: e.into(),
        });
    Some(event)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kiro::model::events::UpstreamStop;

    fn sse(events: &[Value]) -> String {
        events
            .iter()
            .map(|e| format!("event: {}\ndata: {}\n\n", e["type"].as_str().unwrap(), e))
            .collect()
    }

    fn decode_all(body: &str) -> (Vec<Event>, SseEventDecoder) {
        let mut decoder = SseEventDecoder::new();
        let mut events = Vec::new();
        // 逐字节喂入，覆盖行跨 chunk 的情形
        for byte in body.as_bytes() {
            events.extend(decoder.feed(std::slice::from_ref(byte)));
        }
        events.extend(decoder.finish());
        let events = events.into_iter().map(|r| r.ok().unwrap()).collect();
        (events, decoder)
    }

    fn completed(usage: Value) -> Value {
        serde_json::json!({"type": "response.completed", "response": {"status": "completed", "usage": usage}})
    }

    #[test]
    fn text_reasoning_and_usage_are_mapped() {
        let envelope = r#"{"reasoningText":{"text":"hm","signature":"SIG"}}"#;
        let body = sse(&[
            serde_json::json!({"type": "response.created", "response": {}}),
            serde_json::json!({"type": "kiro:keep_alive"}),
            serde_json::json!({"type": "response.reasoning_text.delta", "delta": "hm"}),
            serde_json::json!({"type": "response.output_item.done", "item": {"type": "reasoning", "encrypted_content": envelope}}),
            serde_json::json!({"type": "response.output_text.delta", "delta": "你好"}),
            completed(serde_json::json!({
                "kiro:context_usage_percentage": 12.5,
                "kiro:credits": {"value": 0.42, "unit": "credit", "unit_plural": "credits"}
            })),
        ]);
        let (events, decoder) = decode_all(&body);
        assert!(!decoder.truncated());

        assert!(
            matches!(&events[0], Event::ReasoningContent(r) if r.text == "hm" && r.signature.is_empty())
        );
        assert!(
            matches!(&events[1], Event::ReasoningContent(r) if r.signature == "SIG" && r.text.is_empty())
        );
        assert!(matches!(&events[2], Event::AssistantResponse(a) if a.content == "你好"));
        assert!(
            matches!(&events[3], Event::Metadata(m) if m.upstream_stop() == Some(UpstreamStop::Completed))
        );
        assert!(matches!(&events[4], Event::ContextUsage(c) if c.context_usage_percentage == 12.5));
        assert!(
            matches!(&events[5], Event::Metering(m) if m.usage == 0.42 && m.unit_plural == "credits")
        );
        assert_eq!(events.len(), 6);
    }

    #[test]
    fn streamed_tool_call_carries_id_and_name_on_every_event() {
        let body = sse(&[
            serde_json::json!({"type": "response.output_item.added", "item": {"type": "function_call", "call_id": "tu-1", "name": "Read", "arguments": ""}}),
            serde_json::json!({"type": "response.function_call_arguments.delta", "delta": "{\"path\":"}),
            serde_json::json!({"type": "response.function_call_arguments.delta", "delta": "\"a\"}"}),
            serde_json::json!({"type": "response.function_call_arguments.done", "arguments": "{\"path\":\"a\"}"}),
            serde_json::json!({"type": "response.output_item.done", "item": {"type": "function_call", "call_id": "tu-1", "arguments": "{\"path\":\"a\"}"}}),
            completed(Value::Null),
        ]);
        let (events, _) = decode_all(&body);

        let tool_events: Vec<&ToolUseEvent> = events
            .iter()
            .filter_map(|e| match e {
                Event::ToolUse(t) => Some(t),
                _ => None,
            })
            .collect();
        assert_eq!(tool_events.len(), 4);
        assert!(
            tool_events
                .iter()
                .all(|t| t.tool_use_id == "tu-1" && t.name == "Read")
        );
        let input: String = tool_events.iter().map(|t| t.input.as_str()).collect();
        assert_eq!(input, "{\"path\":\"a\"}");
        assert!(tool_events[3].stop);
        assert!(tool_events[..3].iter().all(|t| !t.stop));
    }

    #[test]
    fn tool_call_without_deltas_gets_full_arguments_and_stop_at_completion() {
        let body = sse(&[
            serde_json::json!({"type": "response.output_item.added", "item": {"type": "function_call", "call_id": "tu-2", "name": "Bash"}}),
            serde_json::json!({"type": "response.output_item.done", "item": {"type": "function_call", "call_id": "tu-2", "arguments": "{\"cmd\":\"ls\"}"}}),
            completed(Value::Null),
        ]);
        let (events, _) = decode_all(&body);

        let inputs: Vec<(String, bool)> = events
            .iter()
            .filter_map(|e| match e {
                Event::ToolUse(t) => Some((t.input.clone(), t.stop)),
                _ => None,
            })
            .collect();
        assert_eq!(
            inputs,
            [
                (String::new(), false),
                ("{\"cmd\":\"ls\"}".to_string(), false),
                (String::new(), true)
            ]
        );
    }

    #[test]
    fn incomplete_maps_stop_reason_and_drops_unfinished_tool_calls() {
        let body = sse(&[
            serde_json::json!({"type": "response.output_item.added", "item": {"type": "function_call", "call_id": "tu-3", "name": "Write"}}),
            serde_json::json!({"type": "response.incomplete", "response": {"status": "incomplete", "incomplete_details": {"reason": "max_output_tokens"}}}),
        ]);
        let (events, decoder) = decode_all(&body);
        assert!(!decoder.truncated());
        assert!(
            !events
                .iter()
                .any(|e| matches!(e, Event::ToolUse(t) if t.stop))
        );
        assert!(
            matches!(events.last().unwrap(), Event::Metadata(m) if m.upstream_stop() == Some(UpstreamStop::MaxTokens))
        );

        let body = sse(&[
            serde_json::json!({"type": "response.incomplete", "response": {"incomplete_details": {"reason": "content_filter"}}}),
        ]);
        let (events, _) = decode_all(&body);
        assert!(
            matches!(&events[0], Event::Metadata(m) if m.upstream_stop() == Some(UpstreamStop::ContentFiltered))
        );
    }

    #[test]
    fn stream_errors_become_error_frames_and_throttling_exceptions() {
        let body = sse(&[
            serde_json::json!({"type": "error", "error": {"type": "too_many_requests", "message": "slow down"}}),
        ]);
        let (events, decoder) = decode_all(&body);
        assert!(!decoder.truncated());
        assert!(
            matches!(&events[0], Event::Exception { exception_type, message }
            if exception_type == THROTTLING_EXCEPTION && message == "slow down")
        );

        // 只给 code 且 code 是错误类别
        let body = sse(&[
            serde_json::json!({"type": "response.failed", "response": {"error": {"code": "server_error"}}}),
        ]);
        let (events, _) = decode_all(&body);
        assert!(
            matches!(&events[0], Event::Error { error_code, error_message }
            if error_code == "server_error" && error_message == "CreateResponse stream failed (server_error)")
        );

        let body = sse(&[
            serde_json::json!({"type": "error", "error": {"type": "invalid_request", "code": "rate_limit_exceeded", "message": "m"}}),
        ]);
        let (events, _) = decode_all(&body);
        assert!(matches!(&events[0], Event::Exception { .. }));
    }

    #[test]
    fn missing_terminal_event_is_truncation() {
        let body =
            sse(&[serde_json::json!({"type": "response.output_text.delta", "delta": "half"})]);
        let (_, decoder) = decode_all(&body);
        assert!(decoder.truncated());
    }

    #[test]
    fn trailing_line_without_newline_is_processed_at_finish() {
        let body = format!("data: {}", completed(Value::Null));
        let mut decoder = SseEventDecoder::new();
        assert!(decoder.feed(body.as_bytes()).is_empty());
        assert_eq!(decoder.finish().len(), 1);
        assert!(!decoder.truncated());
    }

    #[test]
    fn malformed_lines_report_parse_failures() {
        let mut decoder = SseEventDecoder::new();
        let results = decoder.feed(b"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\"\n");
        assert!(matches!(
            &results[0],
            Err(DecodeError::Event { tool_use: true, .. })
        ));

        let results = decoder
            .feed(b"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"x\"}\n");
        assert!(matches!(
            &results[0],
            Err(DecodeError::Event {
                tool_use: true,
                error: ParseError::UnexpectedEvent(_)
            })
        ));

        // 非 data 行与 [DONE] 忽略
        assert!(
            decoder
                .feed(b"event: x\n: comment\ndata: [DONE]\n\n")
                .is_empty()
        );
    }

    #[test]
    fn refusal_text_is_not_duplicated() {
        let body = sse(&[
            serde_json::json!({"type": "response.refusal.delta", "delta": "no"}),
            serde_json::json!({"type": "response.refusal.done", "refusal": "no"}),
        ]);
        let (events, _) = decode_all(&body);
        assert_eq!(events.len(), 1);

        let body = sse(&[serde_json::json!({"type": "response.refusal.done", "refusal": "no"})]);
        let (events, _) = decode_all(&body);
        assert!(matches!(&events[0], Event::AssistantResponse(a) if a.content == "no"));
    }

    #[test]
    fn code_references_are_mapped() {
        let body = sse(&[
            serde_json::json!({"type": "kiro:code_reference.added", "annotations": [
                {"type": "kiro:code_reference", "license_name": "MIT", "url": "https://x"},
                {"type": "url_citation", "url": "https://ignored"}
            ]}),
        ]);
        let (events, _) = decode_all(&body);
        let Event::CodeReference(refs) = &events[0] else {
            panic!("expected code reference");
        };
        assert_eq!(refs.references.len(), 1);
        assert_eq!(refs.references[0].license_name, "MIT");
    }

    #[test]
    fn oversized_line_overflows() {
        let mut decoder = SseEventDecoder::new();
        let results = decoder.feed(&vec![b'a'; MAX_LINE_BYTES + 1]);
        assert!(matches!(
            &results[0],
            Err(DecodeError::Stream(ParseError::BufferOverflow { .. }))
        ));
    }
}
