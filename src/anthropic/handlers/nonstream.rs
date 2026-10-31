// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 非流式响应构建：将 Kiro 事件聚合为完整 Anthropic message 响应（含 thinking 块 flush），用于非流式请求。

use crate::anthropic::stream::{
    CONTENT_LENGTH_EXCEEDED_EXCEPTION, UpstreamTermination, empty_response_oversized_threshold,
    upstream_aborted_message,
};
use crate::anthropic::types::ErrorResponse;
use std::sync::Arc;

use crate::kiro::model::events::{Event, EventType, ReasoningContentEvent, UpstreamStop};
use crate::kiro::parser::decoder::EventStreamDecoder;
use crate::token;
use axum::{
    http::StatusCode,
    response::{IntoResponse, Json, Response},
};
use serde_json::json;
use uuid::Uuid;

use super::error::map_provider_error_with_context;
use super::helpers::strip_json_fences;
use super::stream::{EMPTY_RESPONSE_RETRY_MESSAGE, REFUSAL_ERROR_TYPE};
use crate::anthropic::stream::THINKING_ONLY_TRUNCATED_MESSAGE;

/// 组装非流式响应的 content 数组
///
/// 块顺序与流式路径一致：thinking → text → tool_use。
///
/// 返回 `(content, thinking_only)`。`thinking_only` 为 true 表示整段响应只有
/// thinking、既无可见文本也无工具调用 —— 此时补一个占位空格 text 块，避免客户端
/// 把 content 判定为空响应而卡住（流式路径在 `generate_final_events` 里有等价
/// 兜底）。上游未给收尾理由时调用方据此改返回截断错误。
pub(crate) fn build_non_stream_content(
    thinking_content: &str,
    text_content: &str,
    tool_uses: Vec<serde_json::Value>,
) -> (Vec<serde_json::Value>, bool) {
    let thinking_only =
        !thinking_content.is_empty() && text_content.is_empty() && tool_uses.is_empty();

    let mut content: Vec<serde_json::Value> = Vec::new();

    // 旧文本标签路径没有真实签名，保留原有兼容实现。
    if !thinking_content.is_empty() {
        content.push(json!({
            "type": "thinking",
            "thinking": thinking_content,
            "signature": super::super::stream::generate_fake_signature()
        }));
    }

    let visible = if thinking_only { " " } else { text_content };
    if !visible.is_empty() {
        content.push(json!({
            "type": "text",
            "text": visible
        }));
    }

    content.extend(tool_uses);
    (content, thinking_only)
}

/// 统计传入内容中的 thinking 块 token（BPE 口径）。
/// 无 thinking 块或仅有 0 token 空块
/// （signature-only）时返回 None（未产生有效思考，与流式落库语义一致）。
fn count_thinking_tokens(content: &[serde_json::Value]) -> Option<i32> {
    let mut total = 0i32;
    for block in content {
        if block["type"] == "thinking"
            && let Some(text) = block["thinking"].as_str()
        {
            total += token::count_tokens(text) as i32;
        }
    }
    // 与流式 `(tokens > 0).then_some(tokens)` 对齐：signature-only 空块（0 token）落 None
    (total > 0).then_some(total)
}

/// 返回 (客户端输出 tokens, 计费输出 tokens, thinking 观测值)。
fn non_stream_output_usage(
    content: &[serde_json::Value],
    include_thinking: bool,
) -> (i32, i32, Option<i32>) {
    let thinking_token_count = count_thinking_tokens(content);
    let visible_output_tokens = token::estimate_output_tokens(content);
    let billed_output_tokens = visible_output_tokens + thinking_token_count.unwrap_or(0);
    let output_tokens = if include_thinking {
        billed_output_tokens
    } else {
        visible_output_tokens
    };
    (output_tokens, billed_output_tokens, thinking_token_count)
}

fn flush_native_reasoning(
    pending: &mut ReasoningContentEvent,
    blocks: &mut Vec<serde_json::Value>,
) {
    if pending.text.is_empty() && pending.signature.is_empty() {
        return;
    }
    let reasoning = std::mem::take(pending);
    blocks.push(json!({
        "type": "thinking",
        "thinking": reasoning.text,
        "signature": reasoning.signature
    }));
}

fn collect_native_reasoning(
    event: &Event,
    pending: &mut ReasoningContentEvent,
    blocks: &mut Vec<serde_json::Value>,
) {
    match event {
        Event::ReasoningContent(reasoning) => {
            pending.text.push_str(&reasoning.text);
            pending.signature.push_str(&reasoning.signature);
        }
        Event::AssistantResponse(response) if !response.content.is_empty() => {
            flush_native_reasoning(pending, blocks);
        }
        Event::ToolUse(_) => flush_native_reasoning(pending, blocks),
        _ => {}
    }
}

/// 原生推理块的 credits 反推观测值：`(摘要 tokens, 签名字符数, 带签名块数)`。
/// 须在剔除 thinking 块之前统计（上游已生成即已计费）。
fn native_reasoning_stats(native_blocks: &[serde_json::Value]) -> (i32, usize, usize) {
    let mut summary_tokens = 0i32;
    let mut signature_chars = 0usize;
    let mut signature_blocks = 0usize;
    for block in native_blocks {
        if let Some(text) = block["thinking"].as_str() {
            summary_tokens += token::count_tokens(text) as i32;
        }
        if let Some(signature) = block["signature"].as_str()
            && !signature.is_empty()
        {
            signature_chars += signature.len();
            signature_blocks += 1;
        }
    }
    (summary_tokens, signature_chars, signature_blocks)
}

fn split_non_stream_thinking(text_content: &str, has_native_thinking: bool) -> (String, String) {
    if has_native_thinking {
        (String::new(), text_content.to_string())
    } else {
        super::super::stream::split_thinking_and_visible(text_content)
    }
}

fn build_non_stream_content_with_native(
    mut native_blocks: Vec<serde_json::Value>,
    thinking_content: &str,
    text_content: &str,
    tool_uses: Vec<serde_json::Value>,
) -> (Vec<serde_json::Value>, bool) {
    let native_only = !native_blocks.is_empty() && text_content.is_empty() && tool_uses.is_empty();
    let (mut content, thinking_only) =
        build_non_stream_content(thinking_content, text_content, tool_uses);
    if native_only && content.is_empty() {
        content.push(json!({"type": "text", "text": " "}));
    }
    native_blocks.append(&mut content);
    (native_blocks, thinking_only || native_only)
}

/// 处理非流式请求
#[allow(clippy::too_many_arguments)]
pub(crate) async fn handle_non_stream_request(
    provider: std::sync::Arc<crate::kiro::provider::KiroProvider>,
    request_body: &str,
    model: &str,
    input_tokens: i32,
    prefix_estimated_tokens: i32,
    usage_tracker: Option<std::sync::Arc<crate::model::usage::UsageTracker>>,
    api_key_id: Option<u32>,
    prompt_cache_usage: crate::cache::PromptCacheUsage,
    bound_ids: Vec<u64>,
    client_ip: Option<String>,
    json_schema_requested: bool,
    fp_tracker: Option<std::sync::Arc<crate::cache::fingerprint::FingerprintTracker>>,
    fp_profile: Option<Vec<crate::cache::fingerprint::ContentSegment>>,
    // 是否为 Claude Code /compact 压缩请求（决定上游超时：普通 180s / 压缩 1000s）
    is_compact_request: bool,
    // 实际发往上游的 effort（ConversionResult::effort），随 usage 记录入库
    effort: Option<String>,
    // converter 注入的 thinking 文本标签预算（ConversionResult::thinking_budget_tokens）
    thinking_budget_tokens: Option<i32>,
    // 客户端 User-Agent（随 usage 记录入库）
    user_agent: Option<String>,
    // 断点缓存记账（promptCacheAccounting 开启时为 Some）
    cache_accounting: Option<crate::cache::breakpoint::CacheAccounting>,
    // 会话 conversationId（cache-miss 诊断按会话计算结算间隔与凭据变化）
    conversation_id: Option<String>,
    // 历史中没能回传上游的推理（ConversionResult::history_thinking_loss），随 usage 记录入库
    history_thinking_loss: crate::anthropic::converter::HistoryThinkingLoss,
    // 历史中回传上游的推理签名（ConversionResult::history_reasoning_replay），供 credits 反推
    history_reasoning_replay: crate::anthropic::converter::HistoryReasoningReplay,
    // 客户端请求所用的 API 格式（随 usage 记录入库）
    api_format: crate::model::usage::ApiFormat,
) -> Response {
    // 总耗时起点：进入 handler 即计时（到 record 落库前，口径见 proposal）
    let request_started_at = std::time::Instant::now();
    // 调用 Kiro API（支持多账号故障转移）
    let crate::kiro::provider::ApiResponse {
        response,
        credential_id,
        signature_repair,
        // 持有到函数返回：非流式响应体在返回前已完整生成
        credential_in_flight: _credential_in_flight,
        route,
    } = match provider
        .call_api(request_body, is_compact_request, &bound_ids)
        .await
    {
        Ok(resp) => resp,
        Err(e) => return map_provider_error_with_context(e, model, input_tokens),
    };
    crate::anthropic::converter::remember_rejected_signatures(model, &signature_repair.rejected);

    // 读取响应体
    let body_bytes = match response.bytes().await {
        Ok(bytes) => bytes,
        Err(e) => {
            tracing::error!("读取响应体失败: {}", e);
            return (
                StatusCode::BAD_GATEWAY,
                Json(ErrorResponse::new(
                    "api_error",
                    format!("读取响应失败: {}", e),
                )),
            )
                .into_response();
        }
    };

    // ---- 事件收集状态 ----
    let mut text_content = String::new();
    let mut native_reasoning = ReasoningContentEvent::default();
    let mut native_thinking_blocks = Vec::new();
    let mut tool_uses: Vec<serde_json::Value> = Vec::new();
    let mut has_tool_use = false;
    let mut stop_reason = "end_turn".to_string();
    // 从 contextUsageEvent 计算的实际输入 tokens（已弃用，保留诊断字段恒为 None）
    let context_input_tokens: Option<i32> = None;
    let mut metering_cache_read_tokens: Option<i32> = None;
    let mut metering_cache_creation_tokens: Option<i32> = None;
    let mut metering_usage: Option<f64> = None;

    // 收集工具调用的增量 JSON
    let mut tool_json_buffers: std::collections::HashMap<String, String> =
        std::collections::HashMap::new();

    // 上游异常终止判定（错误帧 / 异常 stopReason / 帧丢失），收集结束后决定是否报错
    let mut termination = UpstreamTermination::default();

    let mut decoder = EventStreamDecoder::new();
    if let Err(e) = decoder.feed(&body_bytes) {
        tracing::warn!("缓冲区溢出: {}", e);
        termination.observe_decode_error(&e);
    }

    for result in decoder.decode_iter() {
        match result {
            Ok(frame) => {
                let tool_use_frame = frame.event_type() == Some(EventType::ToolUse.as_str());
                let parsed = Event::from_frame(frame);
                if let Err(e) = &parsed {
                    termination.observe_parse_failure(tool_use_frame, e);
                }
                if let Ok(event) = parsed {
                    termination.observe_event(&event);
                    collect_native_reasoning(
                        &event,
                        &mut native_reasoning,
                        &mut native_thinking_blocks,
                    );
                    match event {
                        Event::AssistantResponse(resp) => {
                            text_content.push_str(&resp.content);
                        }
                        Event::ToolUse(tool_use) => {
                            has_tool_use = true;

                            // 累积工具的 JSON 输入
                            let buffer = tool_json_buffers
                                .entry(tool_use.tool_use_id.clone())
                                .or_default();
                            buffer.push_str(&tool_use.input);

                            // 如果是完整的工具调用，添加到列表
                            if tool_use.stop {
                                let input: serde_json::Value = if buffer.is_empty() {
                                    serde_json::json!({})
                                } else {
                                    serde_json::from_str(buffer).unwrap_or_else(|e| {
                                        tracing::warn!(
                                            "工具输入 JSON 解析失败: {}, tool_use_id: {}",
                                            e,
                                            tool_use.tool_use_id
                                        );
                                        serde_json::json!({})
                                    })
                                };

                                tool_uses.push(json!({
                                    "type": "tool_use",
                                    "id": tool_use.tool_use_id,
                                    "name": tool_use.name,
                                    "input": input
                                }));
                            }
                        }
                        Event::ContextUsage(context_usage) => {
                            // contextUsage 本地化：弃用 percentage × window 反算，
                            // 仅保留 100% 触发 stop_reason 兜底
                            if context_usage.context_usage_percentage >= 100.0 {
                                stop_reason = "model_context_window_exceeded".to_string();
                            }
                            tracing::debug!(
                                "[deprecated] contextUsageEvent: {:.2}% (仅记录, 不参与 input_tokens 反算)",
                                context_usage.context_usage_percentage,
                            );
                        }
                        Event::Metering(metering) => {
                            metering_cache_read_tokens = metering.cache_read_input_tokens;
                            metering_cache_creation_tokens = metering.cache_creation_input_tokens;
                            metering_usage = Some(metering.usage);
                        }
                        Event::Exception { exception_type, .. }
                            if exception_type == CONTENT_LENGTH_EXCEEDED_EXCEPTION =>
                        {
                            stop_reason = "max_tokens".to_string();
                        }
                        // 异常类 stopReason 由 termination 判定，此处只映射正常收尾的 stop_reason
                        Event::Metadata(metadata) => match metadata.upstream_stop() {
                            Some(UpstreamStop::MaxTokens) => {
                                stop_reason = "max_tokens".to_string();
                            }
                            Some(UpstreamStop::ContextWindowExceeded) => {
                                stop_reason = "model_context_window_exceeded".to_string();
                            }
                            _ => {}
                        },
                        _ => {}
                    }
                }
            }
            Err(e) => {
                tracing::warn!("解码事件失败: {}", e);
                termination.observe_decode_error(&e);
            }
        }
    }
    termination.observe_eof(decoder.buffer_len());
    let feedback = crate::kiro::provider::UpstreamFeedback::new(Arc::clone(&provider), route);
    if let Some(message) = termination.throttle_message() {
        feedback.throttled(message);
    }

    flush_native_reasoning(&mut native_reasoning, &mut native_thinking_blocks);

    // 上游拒答：说明原文原样返回（与流式同口径，不按空响应处理）
    if let Some(message) = termination.refusal() {
        feedback.refused(message);
        return (
            StatusCode::FORBIDDEN,
            Json(ErrorResponse::new(REFUSAL_ERROR_TYPE, message)),
        )
            .into_response();
    }

    // 空响应 = 无正文、无工具调用、无推理（与流式 `is_empty_response` 同口径）。
    // 上下文过大导致的空响应换账号重试也一样：不计入健康度，原样返回
    let empty = text_content.is_empty() && !has_tool_use && native_thinking_blocks.is_empty();
    let oversized_context = input_tokens >= empty_response_oversized_threshold(model);
    let retryable_empty = empty && !oversized_context;

    // 健康度记账：与流式 `report_upstream_outcome_at_eof` 同口径
    if termination.failure().is_none() {
        if !empty {
            feedback.completed();
        } else if retryable_empty {
            feedback.empty_response();
        }
    }

    // 异常终止不得组装成正常响应（客户端会当成任务完成），返回可重试错误
    if let Some(reason) = termination.failure() {
        tracing::warn!(reason = %reason, "上游异常终止，返回错误防止伪装成正常完成");
        return (
            StatusCode::BAD_GATEWAY,
            Json(ErrorResponse::new(
                "overloaded_error",
                upstream_aborted_message(&reason),
            )),
        )
            .into_response();
    }

    // 确定 stop_reason：tool_use 优先级最高，存在工具调用时无条件覆盖
    // max_tokens / model_context_window_exceeded（这些是下一轮才该报告的状态，
    // 不能盖掉本轮的 tool_use，否则客户端只渲染工具块而不执行）。
    if has_tool_use {
        // [TOOLUSE-DIAG] 非流式工具调用收尾诊断：记录覆盖前的原始 stop_reason。
        //
        // 仅在收到过 ToolUse 事件却没拼出任何完整调用时用 warn 上报 —— 此时客户端会
        // 收到 stop_reason=tool_use 但 content 里没有 tool_use 块，即"只显示 call
        // 不执行"的结构特征。正常情况降为 debug，避免每个响应刷一条警告。
        if tool_uses.is_empty() {
            tracing::warn!(
                "[TOOLUSE-DIAG] non_stream 结构异常: has_tool_use=true 但无完整工具调用 \
                 raw_stop_reason={} tool_use_count=0 final_stop_reason=tool_use",
                stop_reason,
            );
        } else {
            tracing::debug!(
                "[TOOLUSE-DIAG] non_stream has_tool_use=true raw_stop_reason={} \
                 tool_use_count={} final_stop_reason=tool_use",
                stop_reason,
                tool_uses.len(),
            );
        }
        stop_reason = "tool_use".to_string();
    }

    // 原生推理已独立收集，正文标签保持原样；仅旧协议需要从正文提取思考。
    let (thinking_content, visible_text) =
        split_non_stream_thinking(&text_content, !native_thinking_blocks.is_empty());
    text_content = visible_text;

    // 响应前缀防护：剔除模型偶发回声用户 `<system-reminder>` 块的情况
    // （见 `stream::echo_guard` 模块文档）。非流式路径已拿到完整文本，
    // 一次性调用即可（无需跨 chunk 状态）。
    text_content = crate::anthropic::stream::ResponseEchoGuard::new().filter(&text_content);

    // JSON schema 结构化输出：去除模型可能添加的 Markdown 代码围栏
    if json_schema_requested && !text_content.is_empty() {
        text_content = strip_json_fences(text_content);
    }

    let (native_reasoning_tokens, signature_chars, signature_blocks) =
        native_reasoning_stats(&native_thinking_blocks);

    // 构建响应内容
    let (content, thinking_only) = build_non_stream_content_with_native(
        native_thinking_blocks,
        &thinking_content,
        &text_content,
        tool_uses,
    );

    let (output_tokens, billed_output_tokens, thinking_token_count) = non_stream_output_usage(
        &content,
        super::super::stream::output_tokens_include_thinking(),
    );

    // 只有 thinking 且上游没给收尾理由（仍是 end_turn）：回复被截断，记账后返回可重试错误。
    // 上游报过 max_tokens / 上下文超限时按该理由正常返回（与流式路径同口径）；
    // tool_use 已在上方覆盖 stop_reason，不会走到截断判定。
    let truncated_after_thinking = thinking_only && stop_reason == "end_turn";

    // contextUsage 本地化后 input_tokens 来源优先级：metering 真值 → 本地 count_all_tokens 估算
    // `context_input_tokens` 已弃用（始终为 None），保留参数仅供 cap_input_tokens 签名兼容
    let _ = context_input_tokens; // 标记已读以避免 unused
    let raw_final_input_tokens = input_tokens;
    let final_input_tokens =
        super::super::stream::cap_input_tokens_pub(raw_final_input_tokens, input_tokens, model);

    // 本地估算 ≥ 1M 兜底触发 stop_reason
    if final_input_tokens >= 1_000_000 && stop_reason == "end_turn" {
        stop_reason = "model_context_window_exceeded".to_string();
    }

    tracing::info!(
        "[input_tokens] 本地化: estimated={} final={}",
        input_tokens,
        final_input_tokens
    );

    // 四层降级链：metering 真值 → prefix 估算 → 指纹追踪 → 比例模拟
    let sim_usage = prompt_cache_usage.scale_to(final_input_tokens);
    let metering_pair = match (metering_cache_read_tokens, metering_cache_creation_tokens) {
        (Some(read), Some(creation)) => Some((read, creation)),
        _ => None,
    };
    let billed_output = crate::model::usage::BilledOutput {
        tokens: billed_output_tokens,
        native_reasoning_tokens,
        signature_chars,
        signature_blocks,
    };
    // 显式注入：handler 始终算出了 prefix_estimated_tokens（可能为 0），
    // 结合 metering_usage 反推校准后以 Some 传入 select_final_usage 选用 prefix 分支
    let replayed_reasoning =
        super::helpers::replayed_reasoning(history_reasoning_replay, &signature_repair);
    let prefix_estimated = Some(crate::model::usage::refine_prefix_cache_read(
        model,
        prefix_estimated_tokens,
        final_input_tokens,
        &replayed_reasoning,
        &billed_output,
        metering_usage,
    ));
    // cache-miss 诊断：credits 反推实际生效（metering 真值未透传、无断点账本）
    // 且命中率低于阈值时输出，量化本次结算并登记会话注册表
    crate::cache::cache_miss_diag::maybe_log(
        conversation_id.as_deref().unwrap_or(""),
        model,
        prefix_estimated_tokens,
        final_input_tokens,
        &replayed_reasoning,
        &billed_output,
        metering_usage,
        // 与实际用量选路（metering_pair）同口径：read/creation 任一缺失即无完整真值
        metering_pair.is_none() && cache_accounting.is_none(),
        credential_id,
        request_started_at,
    );
    let fingerprint_usage = match (fp_tracker.as_ref(), fp_profile.as_ref()) {
        (Some(tracker), Some(profile)) => {
            let account_id = credential_id.to_string();
            tracker.compute(&account_id, profile, final_input_tokens)
        }
        _ => None,
    };
    // 本地写入的提交与上报口径解耦：即便本次用 metering 真值上报也要提交，
    // 否则后续请求回落到本地模拟时（如换到不返回缓存字段的端点）读不到这次的前缀
    if let Some(acc) = &cache_accounting {
        acc.commit();
    }
    let final_usage = match (&cache_accounting, metering_pair) {
        // 断点记账：metering 未透传缓存真值时取代前缀估算
        (Some(acc), None) => acc.usage_for(final_input_tokens),
        _ => crate::cache::select_final_usage(
            final_input_tokens,
            metering_pair,
            prefix_estimated,
            fingerprint_usage,
            sim_usage,
        ),
    };

    // 流结束后写入指纹表（仅当 credential_id 确定）
    if let (Some(tracker), Some(profile)) = (fp_tracker.as_ref(), fp_profile.clone()) {
        let account_id = credential_id.to_string();
        tracker.update(&account_id, profile);
    }

    let report_input = final_usage.input_tokens;
    let report_cache_creation = final_usage.cache_creation_input_tokens;
    let report_cache_read = final_usage.cache_read_input_tokens;
    let report_creation_5m = final_usage.cache_creation_5m_input_tokens;
    let report_creation_1h = final_usage.cache_creation_1h_input_tokens;

    // 记录用量（内部使用真实值）
    if let (Some(tracker), Some(key_id)) = (&usage_tracker, api_key_id) {
        tracing::info!(
            "[usage] 入库: model={} input={} output={} metering_credits={:?} cache_read={} cache_creation={} api_key={} credential=Some({})",
            model,
            final_input_tokens,
            output_tokens,
            metering_usage,
            report_cache_read,
            report_cache_creation,
            key_id,
            credential_id
        );
        tracker.record(
            key_id,
            model.to_string(),
            final_input_tokens,
            output_tokens,
            crate::model::usage::RecordOptions {
                credential_id: Some(credential_id),
                client_ip,
                credits_used: metering_usage,
                cache_read_input_tokens: Some(report_cache_read),
                cache_creation_input_tokens: Some(report_cache_creation),
                effort: effort.clone(),
                thinking_budget_tokens,
                first_token_ms: None,
                duration_ms: Some(request_started_at.elapsed().as_millis() as u64),
                thinking_token_count,
                is_stream: Some(false),
                user_agent,
                thinking_loss: super::helpers::thinking_loss(
                    history_thinking_loss,
                    &signature_repair,
                ),
                api_format: Some(api_format),
            },
        );
    }

    // 空响应与流式一致返回可重试错误（用量已照常入库），不把空内容当成正常完成
    if retryable_empty {
        tracing::warn!(
            est_input_tokens = input_tokens,
            "上游返回空响应（无任何内容事件），返回可重试错误"
        );
        return (
            StatusCode::BAD_GATEWAY,
            Json(ErrorResponse::new(
                "overloaded_error",
                EMPTY_RESPONSE_RETRY_MESSAGE,
            )),
        )
            .into_response();
    }

    if truncated_after_thinking {
        tracing::warn!(
            est_input_tokens = input_tokens,
            "上游在 thinking 之后无理由结束（无正文、无工具调用），返回可重试错误"
        );
        return (
            StatusCode::BAD_GATEWAY,
            Json(ErrorResponse::new(
                "overloaded_error",
                THINKING_ONLY_TRUNCATED_MESSAGE,
            )),
        )
            .into_response();
    }

    // 构建 Anthropic 响应
    let response_body = json!({
        "id": format!("msg_{}", Uuid::new_v4().to_string().replace('-', "")),
        "type": "message",
        "role": "assistant",
        "content": content,
        "model": model,
        "stop_reason": stop_reason,
        "stop_sequence": null,
        // 客户端展示缩放（output_tokens 不缩放）；tracker 已写入真实值。
        // content 构建前已分离 thinking，且 estimate_output_tokens 只累加 text 与
        // tool_use.input（thinking 块的字段名是 "thinking"，不参与统计），
        // 因此 output_tokens 就是可见输出，直接上报真值，不再套 min(380) 上限。
        "usage": {
            "input_tokens": super::super::stream::scale_for_client(report_input, model),
            "output_tokens": output_tokens,
            "cache_creation_input_tokens": super::super::stream::scale_for_client(report_cache_creation, model),
            "cache_read_input_tokens": super::super::stream::scale_for_client(report_cache_read, model),
            "cache_creation": {
                "ephemeral_5m_input_tokens": super::super::stream::scale_for_client(report_creation_5m, model),
                "ephemeral_1h_input_tokens": super::super::stream::scale_for_client(report_creation_1h, model)
            }
        }
    });

    (StatusCode::OK, Json(response_body)).into_response()
}

#[cfg(test)]
mod native_tests {
    use super::*;

    #[test]
    fn test_native_reasoning_nonstream_keeps_signatures_per_block() {
        let mut pending = ReasoningContentEvent::default();
        let mut blocks = Vec::new();
        for (text, signature) in [("第一轮", "sig-1"), ("第二轮", "sig-2")] {
            collect_native_reasoning(
                &Event::ReasoningContent(ReasoningContentEvent {
                    text: text.into(),
                    signature: String::new(),
                }),
                &mut pending,
                &mut blocks,
            );
            collect_native_reasoning(
                &Event::ReasoningContent(ReasoningContentEvent {
                    text: String::new(),
                    signature: signature.into(),
                }),
                &mut pending,
                &mut blocks,
            );
            flush_native_reasoning(&mut pending, &mut blocks);
        }
        let (content, thinking_only) =
            build_non_stream_content_with_native(blocks, "", "答案", Vec::new());
        assert!(!thinking_only);
        assert_eq!(content.len(), 3);
        assert_eq!(content[0]["thinking"], "第一轮");
        assert_eq!(content[0]["signature"], "sig-1");
        assert_eq!(content[1]["thinking"], "第二轮");
        assert_eq!(content[1]["signature"], "sig-2");
        assert_eq!(content[2]["text"], "答案");

        let (content, thinking_only) =
            build_non_stream_content_with_native(vec![content[0].clone()], "", "", Vec::new());
        assert!(thinking_only);
        assert_eq!(content[1]["text"], " ");
    }

    #[test]
    fn test_native_reasoning_nonstream_preserves_body_tags() {
        let body = "<thinking>\n正文中的标签示例</thinking>\n\n答案";
        let native = json!({
            "type": "thinking",
            "thinking": "真实推理",
            "signature": "真实签名"
        });
        let (thinking, text) = split_non_stream_thinking(body, true);
        let (content, thinking_only) = build_non_stream_content_with_native(
            vec![native.clone()],
            &thinking,
            &text,
            Vec::new(),
        );
        assert!(!thinking_only);
        assert_eq!(content, vec![native, json!({"type": "text", "text": body})]);

        let (thinking, text) = split_non_stream_thinking(body, false);
        assert_eq!(thinking.trim(), "正文中的标签示例");
        assert_eq!(text.trim(), "答案");
    }

    /// 非流式 thinking token 统计（无 thinking 返回 None）
    #[test]
    fn test_nonstream_thinking_token_stats() {
        // 含 thinking 块 → Some(>0)
        let native = json!({"type": "thinking", "thinking": "Let me think about this problem carefully.", "signature": "sig"});
        let (content, _) =
            build_non_stream_content_with_native(vec![native], "", "答案", Vec::new());
        let count = count_thinking_tokens(&content);
        assert!(count.unwrap_or(0) > 0, "thinking 块应统计出 >0 的 token 数");

        // 无 thinking 的普通响应 → None
        let (content, _) =
            build_non_stream_content_with_native(Vec::new(), "", "普通回复", Vec::new());
        assert_eq!(count_thinking_tokens(&content), None);

        // signature-only 空块（text 空、signature 非空，flush_native_reasoning 会 push）
        // → None（0 token 不落 Some(0)，与流式「未产生有效思考 → None」对齐）
        let signature_only = json!({"type": "thinking", "thinking": "", "signature": "sig-only"});
        let (content, _) =
            build_non_stream_content_with_native(vec![signature_only], "", "答案", Vec::new());
        assert_eq!(
            count_thinking_tokens(&content),
            None,
            "signature-only 空块应落 None 而非 Some(0)"
        );
    }

    #[test]
    fn test_nonstream_prefix_estimated_refined_with_thinking_billed_output() {
        let final_input_tokens = 53_776;
        let prefix_estimated_tokens = 53_650;
        for native in [false, true] {
            let thinking = "reasoning ".repeat(146);
            let native_blocks = if native {
                vec![json!({"type": "thinking", "thinking": thinking, "signature": "sig"})]
            } else {
                Vec::new()
            };
            let (content, _) = build_non_stream_content_with_native(
                native_blocks,
                if native { "" } else { &thinking },
                "answer",
                Vec::new(),
            );
            let visible = token::estimate_output_tokens(&content);
            let billed = visible + count_thinking_tokens(&content).unwrap();
            // haiku 5.5 计费曲线：cold − 命中比例 · (cold − cached) + 输出
            let input_mtok = final_input_tokens as f64 / 1_000_000.0;
            let cold = 0.00014 + 1.768 * input_mtok;
            let cached = 0.00023 + 0.177 * input_mtok;
            let read_ratio = 20_000.0 / final_input_tokens as f64;
            let credits = cold - read_ratio * (cold - cached) + 8.4 * billed as f64 / 1_000_000.0;
            // 原生块（仅 native 时位于首位）拆出摘要与签名；签名 "sig" 的估算值低于摘要，
            // 上游输出取摘要，与 credits 构造口径一致
            let (native_reasoning_tokens, signature_chars, signature_blocks) =
                native_reasoning_stats(&content[..native as usize]);
            for include_thinking in [false, true] {
                let (output, billed_output, observed) =
                    non_stream_output_usage(&content, include_thinking);
                assert_eq!(billed_output, billed);
                assert_eq!(observed, Some(billed - visible));
                assert_eq!(output, if include_thinking { billed } else { visible });
                let output = crate::model::usage::BilledOutput {
                    tokens: billed_output,
                    native_reasoning_tokens,
                    signature_chars,
                    signature_blocks,
                };
                assert_eq!(
                    crate::model::usage::refine_prefix_cache_read(
                        "claude-haiku-5-5",
                        prefix_estimated_tokens,
                        final_input_tokens,
                        &Default::default(),
                        &output,
                        Some(credits),
                    ),
                    20_000
                );
            }
        }
    }
}
