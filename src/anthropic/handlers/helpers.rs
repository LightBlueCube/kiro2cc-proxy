// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! Handler 杂项工具：剥离 JSON 代码围栏、提取客户端 IP、按模型名覆盖 thinking 开关、/v1/messages/count_tokens 计数端点、suggestion 模式请求识别与快速响应。

use crate::anthropic::converter::is_luna_model;
use crate::anthropic::middleware::ApiKeyContext;
use crate::anthropic::types::{
    CountTokensRequest, CountTokensResponse, ErrorResponse, MessagesRequest, OutputConfig, Thinking,
};

use crate::model::rpm::{InFlightGuard, LimitFull, LocalLimits, RpmTracker};
use crate::token;
use axum::{
    Json as JsonExtractor,
    http::{StatusCode, header},
    response::{IntoResponse, Json},
};
use std::sync::Arc;
use std::time::Duration;

/// API Key 本地限额满时排队的最长时间，超出返回 429
const API_KEY_QUEUE: Duration = Duration::from_secs(10);

/// 并发满时建议客户端的重试间隔（秒）：何时有在途请求结束无从预知，与上游限流的 429 一致
const CONCURRENCY_RETRY_AFTER_SECS: u64 = 5;

/// 请求准入：计入全局与本 Key 的 RPM、并发（未启用监控时返回 `Ok(None)`）
///
/// 本 Key 的 RPM / 并发满时排队，[`API_KEY_QUEUE`] 内仍满则返回 429 + Retry-After。
/// 返回的守卫须持有到响应体发完。
pub(crate) async fn admit_request(
    rpm: Option<&Arc<RpmTracker>>,
    identity: Option<&ApiKeyContext>,
) -> Result<Option<InFlightGuard>, Box<axum::response::Response>> {
    let Some(rpm) = rpm else {
        return Ok(None);
    };
    let key_id = identity.map(|ctx| ctx.id);
    let limits = identity.map_or(LocalLimits::default(), |ctx| ctx.limits);
    let deadline = tokio::time::Instant::now() + API_KEY_QUEUE;
    let full = match rpm.begin_api_key_request_by(key_id, limits, deadline).await {
        Ok(guard) => return Ok(Some(guard)),
        Err(full) => full,
    };

    tracing::warn!(api_key_id = ?key_id, "API Key 本地限额排队超时：{}", full);
    let (retry_after_secs, message) = match full {
        LimitFull::Rpm { retry_after } => (
            retry_after.as_secs(),
            format!(
                "API key rate limit reached: {} requests per minute. Please retry shortly.",
                limits.rpm
            ),
        ),
        LimitFull::Concurrency => (
            CONCURRENCY_RETRY_AFTER_SECS,
            format!(
                "API key concurrency limit reached: {} concurrent requests. Please retry shortly.",
                limits.concurrency
            ),
        ),
    };
    let error = ErrorResponse::new("rate_limit_error", message);
    Err(Box::new(
        (
            StatusCode::TOO_MANY_REQUESTS,
            [(header::RETRY_AFTER, retry_after_secs.to_string())],
            Json(error),
        )
            .into_response(),
    ))
}

/// 模型白名单拦截：Key 无权调用 `model` 时返回 403，放行时返回 None
pub(crate) fn reject_disallowed_model(
    identity: Option<&ApiKeyContext>,
    model: &str,
) -> Option<axum::response::Response> {
    let context = identity?;
    if context.allows_model(model) {
        return None;
    }

    tracing::warn!(api_key_id = context.id, model = %model, "API Key 无权调用该模型");
    let error = ErrorResponse::new(
        "forbidden",
        format!("Model '{}' is not allowed for this API key.", model),
    );
    Some((axum::http::StatusCode::FORBIDDEN, Json(error)).into_response())
}

/// 合并 converter 与 provider 两侧的推理丢失情况，供 usage 记录入库（无丢失为 None）
pub(crate) fn thinking_loss(
    history: crate::anthropic::converter::HistoryThinkingLoss,
    repair: &crate::kiro::provider::SignatureRepair,
) -> Option<crate::model::usage::ThinkingLoss> {
    crate::model::usage::ThinkingLoss {
        unsigned_turns: history.unsigned_turns,
        rendered_turns: history.rendered_turns,
        // 发送前按拒绝缓存丢弃的 + 本次被上游点名删除的
        invalid_turns: history.invalid_turns + repair.rejected.len() as u32,
        signature_rejected: repair.stripped_all,
    }
    .non_empty()
}

/// 实际回传上游的推理签名：扣除 provider 删掉的签名，全部删除时按未回传计
pub(crate) fn replayed_reasoning(
    history: crate::anthropic::converter::HistoryReasoningReplay,
    repair: &crate::kiro::provider::SignatureRepair,
) -> crate::model::usage::ReplayedReasoning {
    if repair.stripped_all {
        return crate::model::usage::ReplayedReasoning::default();
    }
    // 被删的签名都出自 converter 回传的那批，计数必然覆盖它们
    let rejected_chars: usize = repair.rejected.iter().map(String::len).sum();
    crate::model::usage::ReplayedReasoning {
        signature_chars: history.signature_chars - rejected_chars,
        blocks: history.blocks - repair.rejected.len(),
    }
}

/// 去除 JSON 响应中模型可能添加的 Markdown 代码围栏
///
/// 当请求 JSON schema 结构化输出时，部分模型仍会将结果包裹在 ```json...``` 中。
/// 此函数识别并剥离这些围栏，返回纯 JSON 文本。
pub(crate) fn strip_json_fences(text: String) -> String {
    let trimmed = text.trim();
    if !trimmed.starts_with("```") {
        return text;
    }
    let after_fence = if let Some(rest) = trimmed.strip_prefix("```json\n") {
        rest
    } else if let Some(rest) = trimmed.strip_prefix("```json\r\n") {
        rest
    } else if let Some(rest) = trimmed.strip_prefix("```\n") {
        rest
    } else if let Some(rest) = trimmed.strip_prefix("```\r\n") {
        rest
    } else {
        return text;
    };
    let result = after_fence
        .strip_suffix("\n```")
        .or_else(|| after_fence.strip_suffix("\r\n```"))
        .or_else(|| after_fence.strip_suffix("```"))
        .unwrap_or(after_fence);
    result.to_string()
}

/// 从请求头或连接信息提取客户端真实 IP
pub(crate) fn extract_client_ip(
    headers: &axum::http::HeaderMap,
    connect_info: Option<&std::net::SocketAddr>,
) -> Option<String> {
    if let Some(val) = headers.get("x-forwarded-for")
        && let Ok(s) = val.to_str()
    {
        let ip = s.split(',').next().unwrap_or("").trim();
        if !ip.is_empty() {
            return Some(ip.to_string());
        }
    }
    if let Some(val) = headers.get("x-real-ip")
        && let Ok(s) = val.to_str()
    {
        let ip = s.trim();
        if !ip.is_empty() {
            return Some(ip.to_string());
        }
    }
    connect_info.map(|addr| addr.ip().to_string())
}

/// 提取客户端 User-Agent（截断至 256 字符）
///
/// UA 由客户端可控输入，超长值会撑爆落库 JSON 与明细文件，统一截断；
/// 常见 UA（claude-cli/codex 等）远短于该上限，截断不影响正常展示。
/// 非 UTF-8 字节经 lossy 替换（U+FFFD）降级保留可读部分，避免含非 ASCII
/// 的 UA 被整体丢弃、与"未携带"不可区分；chars 截断最坏 4 字节/char
/// （256 char ≤ 1024B），落库体积仍可控。
pub(crate) fn extract_user_agent(headers: &axum::http::HeaderMap) -> Option<String> {
    const MAX_UA_CHARS: usize = 256;
    let value = headers.get(axum::http::header::USER_AGENT)?;
    let ua = String::from_utf8_lossy(value.as_bytes());
    let ua = ua.trim();
    if ua.is_empty() {
        return None;
    }
    Some(ua.chars().take(MAX_UA_CHARS).collect())
}

/// 检测模型名是否包含 "thinking" 后缀，若包含则覆写 thinking 配置
///
/// - Opus 4.6/4.7/4.8/5：覆写为 adaptive 类型
/// - 其他模型：覆写为 enabled 类型
/// - budget_tokens 固定为 20000
pub(crate) fn override_thinking_from_model_name(payload: &mut MessagesRequest) {
    let model_lower = payload.model.to_lowercase();
    if !model_lower.contains("thinking") {
        return;
    }

    let is_opus_adaptive = model_lower.contains("opus")
        && (model_lower.contains("4-6")
            || model_lower.contains("4.6")
            || model_lower.contains("4-7")
            || model_lower.contains("4.7")
            || model_lower.contains("4-8")
            || model_lower.contains("4.8")
            || model_lower.contains("opus-5")
            || model_lower.contains("opus.5")
            || model_lower.contains("opus 5"));

    let thinking_type = if is_opus_adaptive {
        "adaptive"
    } else {
        "enabled"
    };

    tracing::info!(
        model = %payload.model,
        thinking_type = thinking_type,
        "模型名包含 thinking 后缀，覆写 thinking 配置"
    );

    payload.thinking = Some(Thinking {
        thinking_type: thinking_type.to_string(),
        budget_tokens: 20000,
    });

    if is_opus_adaptive {
        payload.output_config = Some(OutputConfig {
            effort: "high".to_string(),
            format: None,
        });
    }
}

/// 判断本次请求是否应启用流式 thinking 处理路径（`<thinking>` 标签的提取与转换）。
///
/// **范围仅限 `gpt-5.6-luna`**，不包括 terra/sol。原因：
/// - `converter::generate_thinking_prefix` / `build_additional_model_request_fields`
///   对全部 GPT 系模型跳过 thinking 注入（有 400 REQUEST_BODY_INVALID 实测依据，
///   范围覆盖整个 gpt-5.6 系列），这一点本函数无需重复处理。
/// - 但"上游是否会真的输出 `<thinking>...</thinking>` 标签、是否具备可用的推理
///   能力"是另一件事，代码库里只有 luna 的实测记录（"上游恒返回 `thinking=0`"，
///   见 README/`openai::model_map` 已知限制）。terra/sol 没有类似证据，不应假定
///   它们也不产出 thinking——若在此处也对它们强制关闭，会误伤其本该具备的、可能
///   仍受客户端 thinking 请求影响的推理能力。
///
/// 因此仅对 luna 强制关闭 `thinking_enabled`：若仍按客户端请求启用，流式状态机会
/// 持续寻找永不出现的 `<thinking>` 标签，而 luna 在缺乏协议约束时有概率自行选择用
/// `<analysis>`/`<summary>` 等自造伪标签组织输出，被当作普通可见文本原样转发给
/// 客户端（配合 `converter::gpt_anti_pseudo_tag_hint` 的提示词引导兜底）。
/// terra/sol 恢复原有行为：`thinking_enabled` 仅取决于客户端是否请求。
pub(crate) fn resolve_thinking_enabled(model: &str, thinking: &Option<Thinking>) -> bool {
    if is_luna_model(model) {
        return false;
    }
    thinking.as_ref().map(|t| t.is_enabled()).unwrap_or(false)
}

/// POST /v1/messages/count_tokens
///
/// 计算消息的 token 数量
pub async fn count_tokens(
    JsonExtractor(payload): JsonExtractor<CountTokensRequest>,
) -> impl IntoResponse {
    tracing::info!(
        model = %payload.model,
        message_count = %payload.messages.len(),
        "Received POST /v1/messages/count_tokens request"
    );

    let total_tokens = token::count_all_tokens(
        payload.model,
        payload.system,
        payload.messages,
        payload.tools,
    ) as i32;

    Json(CountTokensResponse {
        input_tokens: total_tokens.max(1),
    })
}

/// 检测 Claude Code 的输入建议请求（Suggestion Mode）。
///
/// 此类请求是客户端在每轮主对话结束后自动发起的"预测用户下一条输入"辅助请求，
/// 携带完整 50k+ 上下文却只产出几条候选短语。经 Kiro 全量转发会按全价计费
/// （metering ~0.37 credits/次），且其末条消息会导致 history[0] 前缀指纹漂移、
/// 干扰主对话的 prompt cache。这里识别后直接返回空文本响应，不转发上游。
///
/// 判据：最后一条消息为 user 且文本以 `[SUGGESTION MODE` 标记开头（该标记
/// 固定位于消息首部，用 starts_with 而非 contains 降低误伤面）。
pub(crate) fn is_suggestion_mode_request(payload: &MessagesRequest) -> bool {
    payload
        .messages
        .last()
        .filter(|m| m.role == "user")
        .is_some_and(|m| match &m.content {
            serde_json::Value::String(s) => s.starts_with("[SUGGESTION MODE"),
            // 标记固定位于消息首部：仅检查首块，避免首块为普通文本、
            // 后续块恰好以标记开头时误吞整条正常消息
            serde_json::Value::Array(blocks) => blocks.first().is_some_and(|b| {
                b.get("type").and_then(|v| v.as_str()) == Some("text")
                    && b.get("text")
                        .and_then(|t| t.as_str())
                        .is_some_and(|t| t.starts_with("[SUGGESTION MODE"))
            }),
            _ => false,
        })
}

/// 估算缓存前缀 tokens 并按 `prefix_cached` 门控 cache_read 口径。
///
/// 返回 `(raw_prefix_tokens, prefix_estimated_tokens)`：
/// - `raw_prefix_tokens`：system + tools + 除最后一条消息外的全部历史（`count_prefix_tokens`
///   口径），**始终**供 `count_all_tokens_with_prefix` 累加总输入，不受门控影响
/// - `prefix_estimated_tokens`：仅当会话前缀已命中冻结缓存（`ConversionResult::prefix_cached`，
///   详见其文档）时等于 `raw_prefix_tokens`，否则为 0 —— 新窗口首条消息、单条消息重发、
///   history[0] 首次出现/漂移重置的轮次不计入 cache_read
///
/// `/v1/messages` 与 `/cc/v1/messages` 共用，保证两端点口径一致。
pub(crate) fn estimate_prefix_tokens(payload: &MessagesRequest, prefix_cached: bool) -> (u64, i32) {
    let n = payload.messages.len();
    let prior: &[_] = if n > 0 {
        &payload.messages[..n - 1]
    } else {
        &[]
    };
    let raw =
        token::count_prefix_tokens(payload.system.as_deref(), prior, payload.tools.as_deref());
    let gated = if prefix_cached { raw as i32 } else { 0 };
    (raw, gated)
}

/// 为建议请求构造空响应（按 stream 分流）。
///
/// 流式：返回完整 SSE 事件序列（message_start + 空 text 块 + end_turn 收尾）。
/// 非流式：返回等价的 JSON message 对象。
///
/// Claude Code 对该响应只取候选文本，空文本等价于"无建议"；返回结构完整的
/// 正常响应而非错误，避免客户端把建议失败当主对话故障重试。
///
/// `model` 回显请求模型、usage 全部为 0：下游计费网关（如 sub2api）按响应的
/// model / usage 记账，回显假模型名会被标记"模型不一致"，非零 output 会被计费。
pub(crate) fn suggestion_mode_response(stream: bool, model: &str) -> axum::response::Response {
    if !stream {
        return axum::response::Response::builder()
            .status(axum::http::StatusCode::OK)
            .header(axum::http::header::CONTENT_TYPE, "application/json")
            .body(axum::body::Body::from(
                serde_json::json!({
                    "id": "msg_suggestion",
                    "type": "message",
                    "role": "assistant",
                    "content": [{ "type": "text", "text": "" }],
                    "model": model,
                    "stop_reason": "end_turn",
                    "stop_sequence": null,
                    "usage": {
                        "input_tokens": 0,
                        "output_tokens": 0,
                        "cache_creation_input_tokens": 0,
                        "cache_read_input_tokens": 0
                    }
                })
                .to_string(),
            ))
            .unwrap();
    }

    use crate::anthropic::stream::SseEvent;

    let events = [
        SseEvent::new(
            "message_start",
            serde_json::json!({
                "type": "message_start",
                "message": {
                    "id": format!("msg_suggestion_{}", std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .map(|d| d.as_millis())
                        .unwrap_or_default()),
                    "type": "message",
                    "role": "assistant",
                    "content": [],
                    "model": model,
                    "stop_reason": null,
                    "stop_sequence": null,
                    "usage": {
                        "input_tokens": 0,
                        "output_tokens": 0,
                        "cache_creation_input_tokens": 0,
                        "cache_read_input_tokens": 0
                    }
                }
            }),
        ),
        SseEvent::new(
            "content_block_start",
            serde_json::json!({
                "type": "content_block_start",
                "index": 0,
                "content_block": { "type": "text", "text": "" }
            }),
        ),
        SseEvent::new(
            "content_block_stop",
            serde_json::json!({ "type": "content_block_stop", "index": 0 }),
        ),
        SseEvent::new(
            "message_delta",
            serde_json::json!({
                "type": "message_delta",
                "delta": { "stop_reason": "end_turn", "stop_sequence": null },
                "usage": { "input_tokens": 0, "output_tokens": 0 }
            }),
        ),
        SseEvent::new(
            "message_stop",
            serde_json::json!({ "type": "message_stop" }),
        ),
    ];

    let body = events
        .iter()
        .map(|e| e.to_sse_string())
        .collect::<Vec<_>>()
        .join("");

    axum::http::Response::builder()
        .status(axum::http::StatusCode::OK)
        .header(axum::http::header::CONTENT_TYPE, "text/event-stream")
        .header(axum::http::header::CACHE_CONTROL, "no-cache")
        .body(axum::body::Body::from(body))
        .unwrap()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    async fn body_string(resp: axum::response::Response) -> String {
        let bytes = axum::body::to_bytes(resp.into_body(), usize::MAX)
            .await
            .unwrap();
        String::from_utf8(bytes.to_vec()).unwrap()
    }

    fn limited_key(limits: LocalLimits) -> ApiKeyContext {
        ApiKeyContext {
            id: 7,
            bound_credential_ids: None,
            allowed_models: None,
            limits,
        }
    }

    /// 暂停时钟：排队到截止时刻瞬间完成
    #[tokio::test(start_paused = true)]
    async fn admit_request_returns_429_after_queueing_on_full_key() {
        let tracker = Arc::new(RpmTracker::new());
        let key = limited_key(LocalLimits {
            rpm: 0,
            concurrency: 1,
        });
        let _held = admit_request(Some(&tracker), Some(&key)).await.unwrap();

        let started = tokio::time::Instant::now();
        let resp = *admit_request(Some(&tracker), Some(&key))
            .await
            .err()
            .expect("并发已满应拒绝");
        assert_eq!(started.elapsed(), API_KEY_QUEUE);
        assert_eq!(resp.status(), StatusCode::TOO_MANY_REQUESTS);
        assert_eq!(resp.headers()[header::RETRY_AFTER], "5");
        let v: serde_json::Value = serde_json::from_str(&body_string(resp).await).unwrap();
        assert_eq!(v["error"]["type"], "rate_limit_error");

        // 其它 Key、无 Key 请求不受影响
        let other = ApiKeyContext {
            id: 8,
            ..limited_key(LocalLimits::default())
        };
        assert!(admit_request(Some(&tracker), Some(&other)).await.is_ok());
        assert!(admit_request(Some(&tracker), None).await.is_ok());
    }

    #[tokio::test]
    async fn reject_disallowed_model_returns_403_only_outside_whitelist() {
        let ctx = ApiKeyContext {
            id: 1,
            bound_credential_ids: None,
            allowed_models: Some(vec!["claude-opus-4.8".to_string()]),
            limits: Default::default(),
        };
        assert!(reject_disallowed_model(None, "claude-opus-4.7").is_none());
        assert!(reject_disallowed_model(Some(&ctx), "claude-opus-4-8").is_none());

        let resp = reject_disallowed_model(Some(&ctx), "claude-opus-4.7").expect("应拦截");
        assert_eq!(resp.status(), axum::http::StatusCode::FORBIDDEN);
        let v: serde_json::Value = serde_json::from_str(&body_string(resp).await).unwrap();
        assert_eq!(v["error"]["type"], "forbidden");
        assert!(
            v["error"]["message"]
                .as_str()
                .unwrap()
                .contains("claude-opus-4.7")
        );
    }

    #[tokio::test]
    async fn suggestion_response_echoes_model_with_zero_usage() {
        let v: serde_json::Value = serde_json::from_str(
            &body_string(suggestion_mode_response(false, "claude-opus-5-5")).await,
        )
        .unwrap();
        assert_eq!(v["model"], "claude-opus-5-5");
        assert_eq!(v["usage"]["output_tokens"], 0);
        assert_eq!(v["usage"]["input_tokens"], 0);

        let sse = body_string(suggestion_mode_response(true, "claude-opus-5-5")).await;
        assert!(sse.contains("\"model\":\"claude-opus-5-5\""));
        assert!(!sse.contains("\"suggestion\""));
        assert!(!sse.contains("\"output_tokens\":1"));
    }

    fn request_with_last_message(role: &str, content: serde_json::Value) -> MessagesRequest {
        serde_json::from_value(json!({
            "model": "claude-sonnet-4-5",
            "max_tokens": 1024,
            "messages": [
                { "role": "user", "content": "hi" },
                { "role": "assistant", "content": "hello" },
                { "role": role, "content": content }
            ]
        }))
        .unwrap()
    }

    #[test]
    fn suggestion_string_content_hit() {
        let p = request_with_last_message(
            "user",
            json!("[SUGGESTION MODE: Suggest what the user might naturally type next.]"),
        );
        assert!(is_suggestion_mode_request(&p));
    }

    #[test]
    fn suggestion_block_content_hit() {
        let p = request_with_last_message(
            "user",
            json!([{ "type": "text", "text": "[SUGGESTION MODE: suggest next input]" }]),
        );
        assert!(is_suggestion_mode_request(&p));
    }

    #[test]
    fn suggestion_marker_not_at_start_misses() {
        let p = request_with_last_message("user", json!("请问 [SUGGESTION MODE 是什么协议？"));
        assert!(!is_suggestion_mode_request(&p));
    }

    #[test]
    fn assistant_role_misses() {
        let p = request_with_last_message("assistant", json!("[SUGGESTION MODE: ...]"));
        assert!(!is_suggestion_mode_request(&p));
    }

    #[test]
    fn empty_messages_miss() {
        let p: MessagesRequest = serde_json::from_value(json!({
            "model": "claude-sonnet-4-5",
            "max_tokens": 1024,
            "messages": []
        }))
        .unwrap();
        assert!(!is_suggestion_mode_request(&p));
    }

    #[test]
    fn normal_user_message_misses() {
        let p = request_with_last_message("user", json!("帮我看看这个 bug"));
        assert!(!is_suggestion_mode_request(&p));
    }

    #[test]
    fn normal_string_with_marker_inside_misses() {
        // 字符串形态：标记出现在文本中部而非开头，不应误判
        let p =
            request_with_last_message("user", json!("请问 [SUGGESTION MODE] 这个标记是什么意思？"));
        assert!(!is_suggestion_mode_request(&p));
    }

    #[test]
    fn suggestion_marker_in_later_block_misses() {
        // 首块为普通文本、后续块以标记开头：标记不在消息首部，不应误判
        let p = request_with_last_message(
            "user",
            json!([
                { "type": "text", "text": "普通内容" },
                { "type": "text", "text": "[SUGGESTION MODE: suggest next input]" }
            ]),
        );
        assert!(!is_suggestion_mode_request(&p));
    }

    #[test]
    fn suggestion_first_block_non_text_misses() {
        // 首块非 text 类型时即使带标记也不判中（标记必须位于消息首部）
        let p = request_with_last_message(
            "user",
            json!([
                { "type": "image", "text": "[SUGGESTION MODE: x]" },
                { "type": "text", "text": "[SUGGESTION MODE: x]" }
            ]),
        );
        assert!(!is_suggestion_mode_request(&p));
    }

    fn headers_with_ua(ua: &str) -> axum::http::HeaderMap {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert(
            axum::http::header::USER_AGENT,
            axum::http::HeaderValue::from_str(ua).unwrap(),
        );
        headers
    }

    #[test]
    fn extract_user_agent_normal_value_passthrough() {
        let headers = headers_with_ua("claude-cli/2.1.5 (external, cli)");
        assert_eq!(
            extract_user_agent(&headers).as_deref(),
            Some("claude-cli/2.1.5 (external, cli)")
        );
    }

    #[test]
    fn extract_user_agent_missing_or_blank_is_none() {
        // 头缺失 → None
        assert_eq!(extract_user_agent(&axum::http::HeaderMap::new()), None);
        // 仅空白字符（trim 后为空）→ None
        assert_eq!(extract_user_agent(&headers_with_ua("   ")), None);
    }

    #[test]
    fn extract_user_agent_truncated_at_256_chars() {
        let long = "a".repeat(300);
        let result = extract_user_agent(&headers_with_ua(&long)).unwrap();
        assert_eq!(result.len(), 256);
    }

    #[test]
    fn extract_user_agent_surrounding_whitespace_trimmed() {
        let headers = headers_with_ua("  claude-cli/2.1.5  ");
        assert_eq!(
            extract_user_agent(&headers).as_deref(),
            Some("claude-cli/2.1.5")
        );
    }

    #[test]
    fn extract_user_agent_non_ascii_lossy_preserved() {
        // 含非法 UTF-8 字节的 UA：降级保留可读部分（非法字节 → U+FFFD），
        // 不应整体返回 None 与"未携带"混淆
        let value =
            axum::http::HeaderValue::from_bytes(b"claude-cli/2.1.5 (\xFF\xFE, cli)").unwrap();
        let mut headers = axum::http::HeaderMap::new();
        headers.insert(axum::http::header::USER_AGENT, value);
        let result = extract_user_agent(&headers).unwrap();
        assert!(result.starts_with("claude-cli/2.1.5 ("));
        assert!(result.contains('\u{FFFD}'));
        assert!(result.ends_with(", cli)"));
    }

    #[test]
    fn extract_user_agent_valid_utf8_multibyte_preserved() {
        // 合法多字节 UTF-8 UA（如中文标注）应原样保留而非被丢弃
        let headers = headers_with_ua("MyClient/1.0 (中文客户端)");
        assert_eq!(
            extract_user_agent(&headers).as_deref(),
            Some("MyClient/1.0 (中文客户端)")
        );
    }
}
