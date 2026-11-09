// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! Handler 错误处理：构造 prompt_too_long 提示、将 Kiro/provider 错误映射为带上下文的 Anthropic 错误响应、解析请求体为 MessagesRequest。

use crate::anthropic::stream::{CLIENT_ASSUMED_CONTEXT_WINDOW, SseEvent, scale_for_client};
use crate::anthropic::types::{ErrorResponse, MessagesRequest};

use crate::kiro::provider::UpstreamRejected;
use crate::kiro::token_manager::{AccountsCoolingDown, QUOTA_EXHAUSTED_ALL_MARKER};
use anyhow::Error;
use axum::{
    http::{StatusCode, header},
    response::{IntoResponse, Json, Response},
};

/// 超窗错误文案（对齐 Anthropic 官方 `prompt is too long: N tokens > M maximum`）。
///
/// N 取客户端展示口径（`scale_for_client`）、M 取 `CLIENT_ASSUMED_CONTEXT_WINDOW`，
/// 与同一会话中 usage 字段口径一致。N 兜底为 M+1：上游报超窗但本地估算异常偏小
/// （远程 count_tokens 返回 0 等）时，照实填会产出 `0 tokens > 200000 maximum`
/// 这种 N ≤ M 的自相矛盾文案 —— 正是本函数要消除的形态。
pub(crate) fn format_prompt_too_long(estimated_input_tokens: i32, model: &str) -> String {
    let n = scale_for_client(estimated_input_tokens, model).max(CLIENT_ASSUMED_CONTEXT_WINDOW + 1);
    format!(
        "prompt is too long: {} tokens > {} maximum",
        n, CLIENT_ASSUMED_CONTEXT_WINDOW
    )
}

/// 上游限流 429 建议客户端等待的秒数
const RATE_LIMIT_RETRY_AFTER_SECS: u64 = 5;

/// Provider 错误分类结果：HTTP 错误响应与流内 error 事件共用，保证两条路径口径一致
struct ProviderErrorClass {
    status: StatusCode,
    error_type: &'static str,
    message: String,
}

pub(crate) fn map_provider_error_with_context(
    err: Error,
    model: &str,
    estimated_input_tokens: i32,
) -> Response {
    let class = classify_provider_error(&err, model, estimated_input_tokens);
    let body = Json(ErrorResponse::new(class.error_type, class.message));

    // 429 带 Retry-After，交由客户端内置退避接管；账号全部冷却时告知最早可以试探的时间
    if class.status == StatusCode::TOO_MANY_REQUESTS {
        let retry_after = err.downcast_ref::<AccountsCoolingDown>().map_or(
            RATE_LIMIT_RETRY_AFTER_SECS,
            AccountsCoolingDown::retry_after_secs,
        );
        return (
            class.status,
            [(header::RETRY_AFTER, retry_after.to_string())],
            body,
        )
            .into_response();
    }
    (class.status, body).into_response()
}

/// SSE 响应头已下发后（HTTP 状态码已是 200）上游才失败：同口径分类，以流内 error 事件告知客户端
pub(crate) fn provider_error_sse_event(
    err: &Error,
    model: &str,
    estimated_input_tokens: i32,
) -> SseEvent {
    let class = classify_provider_error(err, model, estimated_input_tokens);
    SseEvent::new(
        "error",
        serde_json::json!({
            "type": "error",
            "error": { "type": class.error_type, "message": class.message }
        }),
    )
}

fn classify_provider_error(
    err: &Error,
    model: &str,
    estimated_input_tokens: i32,
) -> ProviderErrorClass {
    let err_str = err.to_string();

    // 上下文窗口满了（对话历史累积超出模型上下文窗口限制）
    if err_str.contains("CONTENT_LENGTH_EXCEEDS_THRESHOLD") {
        tracing::warn!(
            error = %err,
            model = %model,
            estimated_input_tokens = estimated_input_tokens,
            "上游拒绝请求：上下文窗口已满（不应重试）— 请检查是否真正达到 1M 上下文限制"
        );
        // 文案对齐 Anthropic 官方超窗格式 `prompt is too long: N tokens > M maximum`。
        // 原自造文案不匹配任何客户端识别模式，Claude Code 收到后只会硬报错中断（#25）；
        // 官方格式才有机会被识别为「压缩后重试」。两个数字统一用客户端展示口径
        // （N 经 scale_for_client 缩放、M 取客户端假设的 200K 窗口），与同一会话中
        // usage 字段的口径一致，客户端自算 Ctx% 不会与这段文案矛盾。
        return ProviderErrorClass {
            status: StatusCode::BAD_REQUEST,
            error_type: "invalid_request_error",
            message: format_prompt_too_long(estimated_input_tokens, model),
        };
    }

    // 单次输入太长（请求体本身超出上游限制）
    if err_str.contains("Input is too long") {
        tracing::warn!(error = %err, "上游拒绝请求：输入过长（不应重试）");
        return ProviderErrorClass {
            status: StatusCode::BAD_REQUEST,
            error_type: "invalid_request_error",
            message: "Input is too long. Reduce the size of your messages.".to_string(),
        };
    }

    // 上游拒绝请求内容（图片 MIME 不符等）：原样透传状态码与上游报错，
    // 客户端才知道是自己的请求有问题，502 会让它误以为上游故障而反复重试
    if let Some(rejected) = err.downcast_ref::<UpstreamRejected>() {
        tracing::warn!(error = %err, "上游拒绝请求内容（不应重试）：透传状态码与报错");
        return ProviderErrorClass {
            status: rejected.status(),
            error_type: rejected_error_type(rejected.status()),
            message: rejected.client_message(),
        };
    }
    // 额度耗尽（402）：range 内所有（模型过滤后）账号本月额度均已用尽。
    // 必须优先于 429 判断，且只信任 describe_unavailable 产出的机器可识别标记——
    // 该标记只在"scope 内 100% 账号确认为 QuotaExceeded"时才会写入，裸匹配
    // "MONTHLY_REQUEST_COUNT" 会在"单账号耗尽、其余账号仍可用"时被误判为全部耗尽。
    // 额度当月不会恢复，必须返回 402 而非 5xx/429 —— 否则 Claude Code 等客户端会
    // 判定为 temporary 故障并反复重试，定时任务会整轮空转。
    if err_str.contains(QUOTA_EXHAUSTED_ALL_MARKER) {
        tracing::error!(error = %err, "上游额度耗尽：返回 402 告知客户端不可重试");
        return ProviderErrorClass {
            status: StatusCode::PAYMENT_REQUIRED,
            error_type: "quota_exceeded_error",
            message: "All bound Kiro accounts have exhausted their monthly request quota. \
                      Quota resets at the start of next month, or add/enable another account \
                      in the admin panel."
                .to_string(),
        };
    }

    // 候选账号全部冷却、都没到试探时间（或待验证账号的试验名额被占）：本次没有发往上游，
    // 按 429 让客户端等到可以使用再来
    if let Some(cooling) = err.downcast_ref::<AccountsCoolingDown>() {
        tracing::warn!(error = %err, "账号全部冷却或待验证：返回 429，Retry-After 为最早可用时间");
        return ProviderErrorClass {
            status: StatusCode::TOO_MANY_REQUESTS,
            error_type: "rate_limit_error",
            message: format!(
                "All Kiro accounts are cooling down or awaiting verification after repeated \
                 upstream failures. Please retry in {} seconds.",
                cooling.retry_after_secs()
            ),
        };
    }

    // 上游限流（429 Too Many Requests）：所有账号重试后仍被限流。
    // 必须把 429 透传给客户端（而非转成 502），让 Claude Code 等客户端的
    // 内置指数退避重试接管 —— 502 会被客户端判定为硬失败，导致"请求那一轮直接废掉"
    // （表现为工具调用不执行 / 卡住），而 429 会触发客户端自动等待重试。
    if err_str.contains("429") || err_str.contains("Too Many Requests") {
        tracing::warn!(error = %err, "上游限流（所有账号 429 耗尽）：透传 429 给客户端以触发其退避重试");
        return ProviderErrorClass {
            status: StatusCode::TOO_MANY_REQUESTS,
            error_type: "rate_limit_error",
            message: "Upstream rate limit reached on all accounts. Please retry shortly."
                .to_string(),
        };
    }

    // 兜底：完整错误详情只进日志，不回显给客户端——describe_unavailable 等诊断文案
    // 含账号数量/禁用原因拆解，属内部状态，不应通过客户端可见的响应体外泄。
    tracing::error!(error = %err, "Kiro API 调用失败");
    ProviderErrorClass {
        status: StatusCode::BAD_GATEWAY,
        error_type: "api_error",
        message: "Upstream API call failed. Please retry shortly.".to_string(),
    }
}

/// 上游 4xx 对应的 Anthropic 错误类型（401/403/429 在 provider 内另行处理，不会到这里）
fn rejected_error_type(status: StatusCode) -> &'static str {
    match status {
        StatusCode::NOT_FOUND => "not_found_error",
        StatusCode::PAYLOAD_TOO_LARGE => "request_too_large",
        _ => "invalid_request_error",
    }
}

/// 从原始请求体反序列化 MessagesRequest，失败时记录详细的 serde 错误用于诊断。
///
/// 替代 axum 的 `Json<MessagesRequest>` 提取器——后者反序列化失败时直接返回 400
/// 且不记录任何信息，导致无法定位是哪个字段/格式导致客户端请求被拒。
/// 此函数在失败时打印 serde 错误（行列+字段路径）、body 长度、出错行首 12 字符前缀（脱敏）。
#[allow(clippy::result_large_err)]
pub(crate) fn parse_messages_request(body: &[u8]) -> Result<MessagesRequest, Response> {
    match serde_json::from_slice::<MessagesRequest>(body) {
        Ok(req) => Ok(req),
        Err(e) => {
            // serde_json 错误自带行列号；定位出错字节附近的片段辅助判断
            let line = e.line();
            let col = e.column();
            // 估算出错字节偏移附近的上下文（按行列粗略定位，取该行附近 200 字节）
            let body_str = String::from_utf8_lossy(body);
            // 请求体可能含 API Key 与敏感上下文（cr-result C1）：
            // 仅记录出错行首 12 字符前缀做定位线索，不回显完整片段
            let line_prefix: String = body_str
                .lines()
                .nth(line.saturating_sub(1))
                .map(|l| l.chars().take(12).collect())
                .unwrap_or_default();
            tracing::error!(
                error = %e,
                serde_line = line,
                serde_col = col,
                body_len = body.len(),
                line_prefix = %line_prefix,
                "[REQ-DIAG] /v1/messages 请求体反序列化失败（导致 400，客户端那轮中断）"
            );
            Err((
                StatusCode::BAD_REQUEST,
                Json(ErrorResponse::new(
                    "invalid_request_error",
                    format!("Request body could not be parsed: {}", e),
                )),
            )
                .into_response())
        }
    }
}
