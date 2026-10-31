// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 流式请求处理：handle_stream_request 建立 SSE 通道并驱动 Kiro 事件到客户端的转发，wait_deadline 处理全局超时。

use crate::anthropic::stream::{SseEvent, StreamContext, upstream_aborted_message};

use std::convert::Infallible;

use crate::kiro::model::events::{Event, EventType};
use crate::kiro::parser::decoder::EventStreamDecoder;
use axum::{
    body::Body,
    http::{StatusCode, header},
    response::Response,
};
use bytes::Bytes;
use futures::stream::BoxStream;
use futures::{Stream, StreamExt, stream};
use std::pin::Pin;
use std::time::Duration;
use tokio::time::{Instant, Interval, interval_at};

use super::error::{map_provider_error_with_context, provider_error_sse_event};

/// 处理流式请求
#[allow(clippy::too_many_arguments)]
pub(crate) async fn handle_stream_request(
    provider: std::sync::Arc<crate::kiro::provider::KiroProvider>,
    request_body: String,
    model: &str,
    input_tokens: i32,
    prefix_estimated_tokens: i32,
    thinking_enabled: bool,
    usage_tracker: Option<std::sync::Arc<crate::model::usage::UsageTracker>>,
    api_key_id: Option<u32>,
    prompt_cache_usage: crate::cache::PromptCacheUsage,
    bound_ids: Vec<u64>,
    client_ip: Option<String>,
    // 上游流的全局超时；None 表示不限时（/v1 的现有行为）
    stream_deadline: Option<Duration>,
    // 是否为 Claude Code /compact 压缩请求（决定上游超时：普通 180s / 压缩 1000s）
    is_compact_request: bool,
    // 实际发往上游的 effort（ConversionResult::effort），随 usage 记录入库
    effort: Option<String>,
    // converter 注入的 thinking 文本标签预算（ConversionResult::thinking_budget_tokens）
    thinking_budget_tokens: Option<i32>,
    // 思考文本化的渲染样式（配置开启且客户端支持时为 Some）；仅在本次请求启用 thinking 时生效
    thinking_as_text_allowed: Option<crate::anthropic::stream::ThinkingTextStyle>,
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
    // 该 API Key 的在途请求计数：随 SSE 流存活；上游在宽限期内报错时随本函数返回释放
    api_key_in_flight: Option<crate::model::rpm::InFlightGuard>,
) -> Response {
    // 计时起点：发起上游请求之前（与 nonstream.rs 口径一致，对齐 codex2cc 的 TTFT 实现），
    // 首字/耗时均包含上游建连与故障转移耗时；ctx 计时统一用 std Instant，与 tokio 时间源无关
    let request_started_at = std::time::Instant::now();

    // 调用 Kiro API（支持多账号故障转移）；Box::pin 使调用可在宽限期超时后移入 SSE body 继续等待
    let call_provider = std::sync::Arc::clone(&provider);
    let mut upstream_call = Box::pin(async move {
        call_provider
            .call_api_stream(&request_body, is_compact_request, &bound_ids)
            .await
    });

    // 上游响应头到达后构建 SSE 事件流（快慢两条路径共用）
    let stream_model = model.to_string();
    let into_sse_stream = move |upstream: crate::kiro::provider::ApiResponse| {
        let crate::kiro::provider::ApiResponse {
            response,
            credential_id,
            signature_repair,
            credential_in_flight,
            route,
        } = upstream;
        let model = stream_model.as_str();
        crate::anthropic::converter::remember_rejected_signatures(
            model,
            &signature_repair.rejected,
        );

        let thinking_as_text = thinking_as_text_allowed.filter(|_| thinking_enabled);

        // 创建流处理上下文
        let mut ctx = StreamContext::new_with_thinking(model, input_tokens, thinking_enabled)
            .with_usage_tracking(
                usage_tracker,
                api_key_id,
                Some(credential_id),
                client_ip,
                user_agent,
            )
            .with_prompt_cache_usage(prompt_cache_usage)
            .with_prefix_estimated_tokens(prefix_estimated_tokens)
            .with_cache_accounting(cache_accounting)
            .with_conversation_id(conversation_id)
            .with_effort(effort)
            .with_thinking_budget_tokens(thinking_budget_tokens)
            // 计时起点在 call_api_stream 之前注入（见函数开头注释）
            .with_request_started_at(request_started_at)
            .with_thinking_as_text(thinking_as_text)
            .with_thinking_loss(super::helpers::thinking_loss(
                history_thinking_loss,
                &signature_repair,
            ))
            .with_replayed_reasoning(super::helpers::replayed_reasoning(
                history_reasoning_replay,
                &signature_repair,
            ))
            .with_api_format(api_format)
            .with_upstream_feedback(crate::kiro::provider::UpstreamFeedback::new(
                std::sync::Arc::clone(&provider),
                route,
            ));

        // 生成初始事件
        let initial_events = ctx.generate_initial_events();

        // 创建 SSE 流；并发计数随流存活
        let events = create_sse_stream(
            response,
            ctx,
            initial_events,
            stream_deadline.map(|d| Instant::now() + d),
        );
        hold_while_streaming(events, (api_key_in_flight, credential_in_flight))
    };

    // 宽限期内完成（含秒级返回的 400/402/429）→ 原有路径，HTTP 状态码语义不变
    match tokio::time::timeout(UPSTREAM_HEADERS_GRACE, upstream_call.as_mut()).await {
        Ok(Ok(upstream)) => sse_response(into_sse_stream(upstream)),
        Ok(Err(e)) => map_provider_error_with_context(e, model, input_tokens),
        Err(_) => {
            tracing::info!(
                grace_secs = UPSTREAM_HEADERS_GRACE.as_secs(),
                "上游响应头超出宽限期，先下发 SSE 响应头并保活等待"
            );
            sse_response(create_awaiting_upstream_sse_stream(
                upstream_call,
                into_sse_stream,
                model.to_string(),
                input_tokens,
            ))
        }
    }
}

/// 上游响应头宽限期
///
/// 超出后先下发 SSE 响应头并保活，防止反代因首字节超时断开（Cloudflare 默认 125s 即 524）。
/// 确定性错误（400/402/429）通常秒级返回，落在窗口内，仍以 HTTP 状态码告知客户端。
const UPSTREAM_HEADERS_GRACE: Duration = Duration::from_secs(5);

fn sse_response<S>(stream: S) -> Response
where
    S: Stream<Item = Result<Bytes, Infallible>> + Send + 'static,
{
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "text/event-stream")
        .header(header::CACHE_CONTROL, "no-cache")
        .header(header::CONNECTION, "keep-alive")
        .body(Body::from_stream(stream))
        .unwrap()
}

/// 等待上游响应头阶段的 SSE 状态
enum UpstreamPhase<C, B> {
    Waiting {
        upstream_call: Pin<Box<C>>,
        keepalive: Interval,
        into_sse_stream: B,
        model: String,
        input_tokens: i32,
    },
    Streaming(BoxStream<'static, Result<Bytes, Infallible>>),
}

/// 宽限期后仍在等待上游响应头时的 SSE 流
///
/// ```text
/// Waiting ──keepalive tick──▶ ": keepalive"（状态不变）
///    │ 上游 Ok  → into_sse_stream（message_start …）
///    │ 上游 Err → 单个 error 事件（HTTP 状态码已是 200，只能流内告知）
///    ▼
/// Streaming ──▶ 透传至结束
/// ```
pub(crate) fn create_awaiting_upstream_sse_stream<C, T, B, S>(
    upstream_call: Pin<Box<C>>,
    into_sse_stream: B,
    model: String,
    input_tokens: i32,
) -> impl Stream<Item = Result<Bytes, Infallible>>
where
    C: Future<Output = anyhow::Result<T>> + Send + 'static,
    T: Send + 'static,
    B: FnOnce(T) -> S + Send + 'static,
    S: Stream<Item = Result<Bytes, Infallible>> + Send + 'static,
{
    // 首个 tick 立即触发：响应头后紧跟首个 body 字节，不依赖 hyper 在 body 未就绪时单独 flush 响应头
    let keepalive = tokio::time::interval(Duration::from_secs(PING_INTERVAL_SECS));
    let initial = UpstreamPhase::Waiting {
        upstream_call,
        keepalive,
        into_sse_stream,
        model,
        input_tokens,
    };

    stream::unfold(initial, |phase| async move {
        match phase {
            UpstreamPhase::Waiting {
                mut upstream_call,
                mut keepalive,
                into_sse_stream,
                model,
                input_tokens,
            } => {
                tokio::select! {
                    result = upstream_call.as_mut() => {
                        let mut events = match result {
                            Ok(upstream) => into_sse_stream(upstream).boxed(),
                            Err(e) => {
                                let event = provider_error_sse_event(&e, &model, input_tokens);
                                let frame = Ok::<_, Infallible>(Bytes::from(event.to_sse_string()));
                                stream::iter([frame]).boxed()
                            }
                        };
                        let first = events.next().await?;
                        Some((first, UpstreamPhase::Streaming(events)))
                    }
                    _ = keepalive.tick() => {
                        tracing::trace!("等待上游响应头，发送注释保活");
                        let next = UpstreamPhase::Waiting {
                            upstream_call,
                            keepalive,
                            into_sse_stream,
                            model,
                            input_tokens,
                        };
                        Some((Ok(keepalive_comment_sse()), next))
                    }
                }
            }
            UpstreamPhase::Streaming(mut events) => {
                let item = events.next().await?;
                Some((item, UpstreamPhase::Streaming(events)))
            }
        }
    })
}

/// 让 `guards` 随流存活：流结束或客户端断开（流被 drop）时一并释放
fn hold_while_streaming<S, G>(stream: S, guards: G) -> impl Stream<Item = S::Item>
where
    S: Stream,
    G: Send + 'static,
{
    stream.map(move |item| {
        let _ = &guards;
        item
    })
}

/// SSE 注释帧：协议规定客户端忽略
///
/// 用于 message_start 之前的保活——Anthropic 事件顺序以 message_start 开头，此时不宜发 ping 事件。
pub(crate) fn keepalive_comment_sse() -> Bytes {
    Bytes::from_static(b": keepalive\n\n")
}

/// Ping 事件间隔（25秒）
const PING_INTERVAL_SECS: u64 = 25;

/// 等待全局 deadline；`None` 时永不就绪，使调用方的 `select!` 分支等价于不存在
///
/// 不用 `select!` 的 `if` precondition：分支的 future 表达式必须无论 precondition
/// 真假都能构造，而 `sleep_until` 需要已解包的 `Instant`，那样得凭空造一个
/// 「很远的未来」哨兵值。
pub(crate) async fn wait_deadline(deadline: Option<Instant>) {
    match deadline {
        Some(d) => tokio::time::sleep_until(d).await,
        None => std::future::pending::<()>().await,
    }
}

/// 创建 ping 事件的 SSE 字符串
pub(crate) fn create_ping_sse() -> Bytes {
    Bytes::from("event: ping\ndata: {\"type\": \"ping\"}\n\n")
}

/// 小输入空响应的错误文案（流式 SSE error 事件与非流式错误响应共用）
pub(super) const EMPTY_RESPONSE_RETRY_MESSAGE: &str =
    "Upstream returned an empty response. Please retry.";

/// 为上游空响应构造合适的 SSE error 事件。
///
/// - 大输入（疑似上下文过大）：返回 invalid_request_error，提示压缩上下文，
///   不鼓励原样重试（重试还是同样的大请求，仍会空）。
/// - 小输入（疑似偶发）：返回 overloaded_error，客户端可重试。
fn empty_response_error_event(oversized_context: bool) -> SseEvent {
    let (err_type, message) = if oversized_context {
        (
            "invalid_request_error",
            "Upstream returned an empty response, likely because the context is too large. \
             Reduce conversation history (e.g. /compact), system prompt, or tools, then retry.",
        )
    } else {
        ("overloaded_error", EMPTY_RESPONSE_RETRY_MESSAGE)
    };
    SseEvent::new(
        "error",
        serde_json::json!({
            "type": "error",
            "error": { "type": err_type, "message": message }
        }),
    )
}

/// 上游读流出现传输层错误（解码失败/连接中断/超时）且已产生部分内容时的 error 事件。
///
/// `Err` 只可能来自传输层异常——正常完成只通过 `None`（EOF）传达，见
/// `StreamContext::is_empty_response` 文档。因此哪怕此前已经产生了部分内容（thinking/text/
/// tool_use），也不能用 `generate_final_events`/`finish_and_get_all_events` 把中断伪装成正常的
/// end_turn/tool_use 完成，否则客户端会把截断的响应当成任务已完成而停止推进，只能靠用户手动
/// 重新输入才能恢复，且不会自动重试。
///
/// 类型为 `overloaded_error`（保持客户端可重试语义，偶发的上游抖动重试仍然有效），
/// message 中带上流断开时已持续的秒数，便于用户/客户端日志对照代理日志的
/// `stream_elapsed_secs` 排查（见 issue #46）。
///
/// 有意**不**在 message 中断言断流原因：#46 的 ~247s 恒定重置根因未确认，实测 361s 的
/// 流可正常完成，已排除"无条件的单次流总时长上限"，但 thinking 路径看门狗、端点差异、
/// 账号/网络链路等解释均未证实，不能据此引导用户做可能无效的操作（如缩短输出）。
pub(crate) fn stream_interrupted_error_event_after(elapsed: Duration) -> SseEvent {
    let secs = elapsed.as_secs();
    SseEvent::new(
        "error",
        serde_json::json!({
            "type": "error",
            "error": {
                "type": "overloaded_error",
                "message": format!(
                    "Upstream connection was interrupted before the response finished \
                     (after {secs}s). Please retry."
                )
            }
        }),
    )
}

/// 上游异常终止（见 `UpstreamTermination`）时的 error 事件：overloaded_error 保持可重试语义
pub(crate) fn upstream_aborted_error_event(reason: &str) -> SseEvent {
    SseEvent::new(
        "error",
        serde_json::json!({
            "type": "error",
            "error": {
                "type": "overloaded_error",
                "message": upstream_aborted_message(reason)
            }
        }),
    )
}

/// /cc 全局 deadline 触发时返回给客户端的 error 事件（两处 deadline 收尾路径共用）
pub(crate) fn deadline_error_event() -> SseEvent {
    SseEvent::new(
        "error",
        serde_json::json!({
            "type": "error",
            "error": {
                "type": "overloaded_error",
                "message": "Upstream response timed out (streaming mode deadline)"
            }
        }),
    )
}

/// 创建 SSE 事件流
fn create_sse_stream(
    response: reqwest::Response,
    ctx: StreamContext,
    initial_events: Vec<SseEvent>,
    deadline: Option<Instant>,
) -> impl Stream<Item = Result<Bytes, Infallible>> {
    // 先发送初始事件
    let initial_stream = stream::iter(
        initial_events
            .into_iter()
            .map(|e| Ok(Bytes::from(e.to_sse_string()))),
    );

    // 然后处理 Kiro 响应流，同时每25秒发送 ping 保活
    let body_stream = response.bytes_stream();
    // 流起点（上游响应头已到达）：读流失败时用于诊断/提示"流已持续多久"
    let stream_started_at = Instant::now();

    let processing_stream = stream::unfold(
        (body_stream, ctx, EventStreamDecoder::new(), false, interval_at(Instant::now() + Duration::from_secs(PING_INTERVAL_SECS), Duration::from_secs(PING_INTERVAL_SECS)), deadline),
        move |(mut body_stream, mut ctx, mut decoder, finished, mut ping_interval, deadline)| async move {
            if finished {
                return None;
            }

            // 使用 select! 同时等待数据、ping 定时器与全局 deadline。
            // 有意不加 biased：不加时 select! 对同时就绪的分支做随机选择，任一分支被
            // 连续跳过的概率指数衰减，ping 与 deadline 都不会被密集 chunk 饿死。
            // （已删除的 create_buffered_sse_stream 需要 biased，是因为它在单次 poll
            //  内用显式 loop 反复 select 且 chunk 分支不返回 —— 那才是确定性饿死源。）
            // 加 biased 会改变 /v1 现有的分支优先级。
            tokio::select! {
                // 处理数据流
                chunk_result = body_stream.next() => {
                    match chunk_result {
                        Some(Ok(chunk)) => {
                            // 解码事件
                            if let Err(e) = decoder.feed(&chunk) {
                                tracing::warn!("缓冲区溢出: {}", e);
                                ctx.termination.observe_decode_error(&e);
                            }

                            let mut events = Vec::new();
                            for result in decoder.decode_iter() {
                                match result {
                                    Ok(frame) => {
                                        let tool_use_frame =
                                            frame.event_type() == Some(EventType::ToolUse.as_str());
                                        match Event::from_frame(frame) {
                                            Ok(event) => {
                                                ctx.termination.observe_event(&event);
                                                ctx.report_upstream_throttle();
                                                let sse_events = ctx.process_kiro_event(&event);
                                                // 首字埋点：按「本帧是否产出客户端可见的 SSE 事件」
                                                // 判定，而非按事件变体名 —— 空 content / thinking 关闭的
                                                // ReasoningContent 等早退分支返回空事件，客户端零输出不应
                                                // 计首字；Metering/ContextUsage 等元信息帧天然产出空事件，
                                                // keepalive 是 SSE 层合成事件，同样不走此处
                                                if !sse_events.is_empty() {
                                                    ctx.mark_first_token();
                                                }
                                                events.extend(sse_events);
                                            }
                                            Err(e) => {
                                                ctx.termination.observe_parse_failure(tool_use_frame, &e);
                                            }
                                        }
                                    }
                                    Err(e) => {
                                        tracing::warn!("解码事件失败: {}", e);
                                        ctx.termination.observe_decode_error(&e);
                                    }
                                }
                            }

                            // 转换为 SSE 字节流
                            let bytes: Vec<Result<Bytes, Infallible>> = events
                                .into_iter()
                                .map(|e| Ok(Bytes::from(e.to_sse_string())))
                                .collect();

                            Some((stream::iter(bytes), (body_stream, ctx, decoder, false, ping_interval, deadline)))
                        }
                        Some(Err(e)) => {
                            let e = e.without_url();
                            tracing::error!(
                                error = ?e,
                                is_timeout = e.is_timeout(),
                                is_body = e.is_body(),
                                is_decode = e.is_decode(),
                                stream_elapsed_secs = stream_started_at.elapsed().as_secs(),
                                "读取响应流失败"
                            );
                            let final_events = if ctx.is_empty_response() {
                                let oversized = ctx.empty_response_is_oversized_context();
                                tracing::warn!(
                                    oversized_context = oversized,
                                    est_input_tokens = ctx.input_tokens,
                                    "流解码错误且无内容，补发 error 事件"
                                );
                                if oversized {
                                    ctx.generate_final_events()
                                } else {
                                    vec![empty_response_error_event(false)]
                                }
                            } else {
                                let elapsed = stream_started_at.elapsed();
                                tracing::warn!(
                                    est_input_tokens = ctx.input_tokens,
                                    stream_elapsed_secs = elapsed.as_secs(),
                                    "流读取错误但已产生部分内容，补发 error 事件防止伪装成正常完成"
                                );
                                vec![stream_interrupted_error_event_after(elapsed)]
                            };
                            let bytes: Vec<Result<Bytes, Infallible>> = final_events
                                .into_iter()
                                .map(|e| Ok(Bytes::from(e.to_sse_string())))
                                .collect();
                            Some((stream::iter(bytes), (body_stream, ctx, decoder, true, ping_interval, deadline)))
                        }
                        None => {
                            ctx.termination.observe_eof(decoder.buffer_len());
                            ctx.report_upstream_outcome_at_eof();
                            let mut out_events = Vec::new();
                            if let Some(reason) = ctx.termination.failure() {
                                tracing::warn!(
                                    reason = %reason,
                                    est_input_tokens = ctx.input_tokens,
                                    "上游异常终止，补发 error 事件防止伪装成正常完成"
                                );
                                out_events.push(upstream_aborted_error_event(&reason));
                            } else if ctx.is_empty_response() {
                                let oversized = ctx.empty_response_is_oversized_context();
                                tracing::warn!(
                                    oversized_context = oversized,
                                    est_input_tokens = ctx.input_tokens,
                                    "上游返回空响应（无任何内容事件），补发 error 事件"
                                );
                                if oversized {
                                    out_events = ctx.generate_final_events();
                                } else {
                                    out_events.push(empty_response_error_event(false));
                                }
                            } else {
                                out_events = ctx.generate_final_events();
                            }
                            let bytes: Vec<Result<Bytes, Infallible>> = out_events
                                .into_iter()
                                .map(|e| Ok(Bytes::from(e.to_sse_string())))
                                .collect();
                            Some((stream::iter(bytes), (body_stream, ctx, decoder, true, ping_interval, deadline)))
                        }
                    }
                }
                // 发送 ping 保活
                _ = ping_interval.tick() => {
                    tracing::trace!("发送 ping 保活事件");
                    let bytes: Vec<Result<Bytes, Infallible>> = vec![Ok(create_ping_sse())];
                    Some((stream::iter(bytes), (body_stream, ctx, decoder, false, ping_interval, deadline)))
                }
                // 全局 deadline：防止上游挂起导致请求永不结束（deadline 为 None 时永不就绪）
                _ = wait_deadline(deadline) => {
                    tracing::error!("流式转发全局超时，强制终止");
                    let bytes = vec![Ok(Bytes::from(deadline_error_event().to_sse_string()))];
                    Some((stream::iter(bytes), (body_stream, ctx, decoder, true, ping_interval, deadline)))
                }
            }
        },
    )
    .flatten();

    initial_stream.chain(processing_stream)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::rpm::RpmTracker;
    use std::sync::Arc;

    #[tokio::test]
    async fn held_guard_is_released_only_when_stream_is_dropped() {
        let tracker = Arc::new(RpmTracker::new());
        let guard = tracker
            .try_begin_api_key_request(Some(1), Default::default())
            .unwrap();
        let mut held = Box::pin(hold_while_streaming(stream::iter([1, 2]), guard));

        // 流读尽前后都不释放：释放时机是流被 drop（正常结束或客户端断开）
        assert_eq!(held.next().await, Some(1));
        assert_eq!(held.next().await, Some(2));
        assert_eq!(held.next().await, None);
        assert_eq!(tracker.snapshot().concurrency_global, 1);

        drop(held);
        assert_eq!(tracker.snapshot().concurrency_global, 0);
    }
}
