//! StreamContext 主结构：聚合单次流式请求的全部状态（事件缓冲、块状态、usage 统计），是 SSE 状态机的上下文载体。
use std::collections::HashMap;
use std::sync::Arc;
use std::time::Instant;

use serde_json::json;
use uuid::Uuid;

use crate::kiro::model::events::{Event, UpstreamStop};
use crate::model::usage::{UsageError, UsageTracker};

use super::outcome::{ResponseOutcome, should_record_usage};

use super::calib::{cap_input_tokens, scale_for_client};
use super::state::{SseEvent, SseStateManager};
use super::thinking::{
    find_char_boundary, find_real_thinking_end_tag, find_real_thinking_end_tag_at_buffer_end,
    find_real_thinking_start_tag,
};

mod metrics;
mod native;
mod tool_use;

/// 流处理上下文
pub struct StreamContext {
    /// SSE 状态管理器
    pub state_manager: SseStateManager,
    /// 请求的模型名称
    pub model: String,
    /// 消息 ID
    pub message_id: String,
    /// 输入 tokens（估算值）
    pub input_tokens: i32,
    /// 从 contextUsageEvent 计算的实际输入 tokens
    pub context_input_tokens: Option<i32>,
    /// 计费口径的输出全文缓冲（含 thinking），收尾一次性 BPE 编码。见 `output_tokens()`
    pub output_full_text: String,
    /// 上报口径的输出全文缓冲（不含 thinking），收尾一次性 BPE 编码。见 `visible_output_tokens()`
    pub visible_full_text: String,
    /// 客户端实际收到的 thinking 全文缓存（BPE 真值统计口径，
    /// 落库时一次性 count_tokens）。见 `thinking_token_count()`
    pub thinking_full_text: String,
    /// 工具块索引映射 (tool_id -> block_index)
    pub tool_block_indices: HashMap<String, i32>,
    /// thinking 是否启用
    pub thinking_enabled: bool,
    /// thinking 内容缓冲区
    pub thinking_buffer: String,
    /// 是否在 thinking 块内
    pub in_thinking_block: bool,
    /// thinking 块是否已提取完成
    pub thinking_extracted: bool,
    /// thinking 块索引
    pub thinking_block_index: Option<i32>,
    /// 文本块索引（thinking 启用时动态分配）
    pub text_block_index: Option<i32>,
    /// 是否需要剥离 thinking 内容开头的换行符
    /// 模型输出 `<thinking>\n` 时，`\n` 可能与标签在同一 chunk 或下一 chunk
    strip_thinking_leading_newline: bool,
    /// signature_delta 是否已发送（签名只能在 content_block_stop 之前发送一次）
    signature_sent: bool,
    /// 原生推理块独立于旧文本标签状态。
    native_thinking_index: Option<i32>,
    native_thinking_seen: bool,
    /// 上游原生推理摘要全文（含账号关闭 thinking 时未下发的部分），credits 反推用
    native_reasoning_full_text: String,
    /// 上游原生推理签名字符数之和与带签名事件数，credits 反推估算隐藏推理输出用
    native_signature_chars: usize,
    native_signature_blocks: usize,
    /// 上一个推理事件是否带签名（同一块的签名分片不重复计块）
    in_native_signature_run: bool,
    /// 用量追踪器（可选）
    usage_tracker: Option<Arc<UsageTracker>>,
    /// API Key ID（用于用量记录）
    api_key_id: Option<u32>,
    /// 账号 ID（用于用量记录）
    credential_id: Option<u64>,
    /// 客户端 IP（用于用量记录）
    client_ip: Option<String>,
    /// 客户端 User-Agent（用于用量记录）
    user_agent: Option<String>,
    /// 从 meteringEvent 获取的真实 credits 消耗
    pub metering_usage: Option<f64>,
    /// 从 meteringEvent 获取的 cache read tokens（Kiro 透传时有值）
    metering_cache_read_tokens: Option<i32>,
    /// 从 contextUsageEvent 获取的上下文使用百分比（0-100）
    context_usage_percentage: Option<f64>,
    /// 前缀命中观测（收尾时结算一次，见 `crate::cache`）
    prefix_probe: Option<crate::cache::PrefixProbe>,
    /// 实际发往上游的 effort，随 usage 记录入库
    effort: Option<String>,
    /// 实际生效的 thinking 文本标签预算，随 usage 记录入库
    thinking_budget_tokens: Option<i32>,
    /// 历史推理丢失情况，随 usage 记录入库
    thinking_loss: Option<crate::model::usage::ThinkingLoss>,
    /// 历史中回传上游的推理签名，供 credits 反推
    replayed_reasoning: crate::model::usage::ReplayedReasoning,
    /// 客户端请求所用的 API 格式，随 usage 记录入库
    api_format: Option<crate::model::usage::ApiFormat>,
    /// 思考文本化转换器（None = 关闭，出站事件原样输出）
    thinking_text: Option<crate::anthropic::stream::ThinkingTextRewriter>,
    /// 响应前缀防护：剔除模型偶发回声用户 `<system-reminder>` 块的情况（见模块文档）
    echo_guard: super::echo_guard::ResponseEchoGuard,
    /// 请求计时起点（进入 handler 时注入；None = 未注入，落库时不计耗时）
    pub(crate) request_started_at: Option<Instant>,
    /// 首个上游业务事件时刻（keepalive 不计；None = 尚未收到或未注入起点）
    pub(crate) first_token_at: Option<Instant>,
    /// 上游异常终止判定（handler 逐个喂入事件，流结束时据此决定收尾方式）
    pub(crate) termination: super::UpstreamTermination,
    /// 上游结果回报（限流帧、拒答、异常结束、正常完成），由 handler 注入；None = 不回报（测试）
    upstream_feedback: Option<crate::kiro::provider::UpstreamFeedback>,
    /// 限流帧已回报过
    throttle_reported: bool,
}

impl StreamContext {
    /// 创建启用thinking的StreamContext
    pub fn new_with_thinking(
        model: impl Into<String>,
        input_tokens: i32,
        thinking_enabled: bool,
    ) -> Self {
        Self {
            state_manager: SseStateManager::new(),
            thinking_text: None,
            model: model.into(),
            message_id: format!("msg_{}", Uuid::new_v4().to_string().replace('-', "")),
            input_tokens,
            context_input_tokens: None,
            output_full_text: String::new(),
            visible_full_text: String::new(),
            thinking_full_text: String::new(),
            tool_block_indices: HashMap::new(),
            thinking_enabled,
            thinking_buffer: String::new(),
            in_thinking_block: false,
            thinking_extracted: false,
            thinking_block_index: None,
            text_block_index: None,
            strip_thinking_leading_newline: false,
            signature_sent: false,
            native_thinking_index: None,
            native_thinking_seen: false,
            native_reasoning_full_text: String::new(),
            native_signature_chars: 0,
            native_signature_blocks: 0,
            in_native_signature_run: false,
            usage_tracker: None,
            api_key_id: None,
            credential_id: None,
            client_ip: None,
            user_agent: None,
            metering_usage: None,
            metering_cache_read_tokens: None,
            context_usage_percentage: None,
            prefix_probe: None,
            effort: None,
            thinking_budget_tokens: None,
            thinking_loss: None,
            replayed_reasoning: Default::default(),
            api_format: None,
            echo_guard: super::echo_guard::ResponseEchoGuard::new(),
            request_started_at: None,
            first_token_at: None,
            upstream_feedback: None,
            throttle_reported: false,
            termination: Default::default(),
        }
    }

    /// 注入请求计时起点（进入 handler 时调用；首个客户端可见 SSE 产出时计算首字延迟）
    pub fn with_request_started_at(mut self, started_at: Instant) -> Self {
        self.request_started_at = Some(started_at);
        self
    }

    /// 标记首个客户端可见 SSE 产出的时刻（幂等：仅首次调用生效；空输出帧与
    /// keepalive 不经过此路径，调用侧按「本帧是否产出非空 SSE 事件」判定）
    pub fn mark_first_token(&mut self) {
        if self.first_token_at.is_none() {
            self.first_token_at = Some(Instant::now());
        }
    }

    /// 注入前缀命中观测（None：本次不按前缀估算 cache_read）
    pub fn with_prefix_probe(mut self, probe: Option<crate::cache::PrefixProbe>) -> Self {
        self.prefix_probe = probe;
        self
    }

    /// 开启思考文本化：出站 SSE 的 thinking 块改写为 text 块（见 `thinking_text` 模块）
    pub fn with_thinking_as_text(
        mut self,
        style: Option<crate::anthropic::stream::ThinkingTextStyle>,
    ) -> Self {
        self.thinking_text = style.map(crate::anthropic::stream::ThinkingTextRewriter::with_style);
        self
    }

    /// 出站事件统一经此处理：开启文本化时改写 thinking 块，否则原样返回
    fn render_outbound(&mut self, events: Vec<SseEvent>) -> Vec<SseEvent> {
        match self.thinking_text.as_mut() {
            Some(rewriter) => rewriter.rewrite(events),
            None => events,
        }
    }

    /// 设置 effort 级别（随 usage 记录入库）
    pub fn with_effort(mut self, effort: Option<String>) -> Self {
        self.effort = effort;
        self
    }

    /// 设置 thinking 预算 tokens（随 usage 记录入库）
    pub fn with_thinking_budget_tokens(mut self, budget: Option<i32>) -> Self {
        self.thinking_budget_tokens = budget;
        self
    }

    /// 设置历史推理丢失情况（随 usage 记录入库）
    pub fn with_thinking_loss(mut self, loss: Option<crate::model::usage::ThinkingLoss>) -> Self {
        self.thinking_loss = loss;
        self
    }

    /// 设置历史中回传上游的推理签名（credits 反推计入回传推理的输入计费）
    pub fn with_replayed_reasoning(
        mut self,
        replayed: crate::model::usage::ReplayedReasoning,
    ) -> Self {
        self.replayed_reasoning = replayed;
        self
    }

    /// 设置客户端请求所用的 API 格式（随 usage 记录入库）
    pub fn with_api_format(mut self, api_format: crate::model::usage::ApiFormat) -> Self {
        self.api_format = Some(api_format);
        self
    }

    /// 设置上游结果的回报对象（限流帧、拒答、异常结束、正常完成）
    pub fn with_upstream_feedback(
        mut self,
        feedback: crate::kiro::provider::UpstreamFeedback,
    ) -> Self {
        self.upstream_feedback = Some(feedback);
        self
    }

    /// 已收到限流帧则回报一次；每个上游事件后调用，流怎么结束（EOF、读错误、客户端断开）都不漏报
    pub(crate) fn report_upstream_throttle(&mut self) {
        if self.throttle_reported {
            return;
        }
        let (Some(message), Some(feedback)) =
            (self.termination.throttle_message(), &self.upstream_feedback)
        else {
            return;
        };
        feedback.throttled(message);
        self.throttle_reported = true;
    }

    /// 上游流读到 EOF：判定结局、回报账号并产出收尾事件（须在 `termination.observe_eof` 之后调用）
    ///
    /// 出错的结局只发 error 事件，不伪装成正常收尾；口径见 [`ResponseOutcome`]。
    pub(crate) fn finish_at_eof(&mut self) -> Vec<SseEvent> {
        let failed = ResponseOutcome::before_content(
            &self.termination,
            self.is_empty_response(),
            self.empty_response_is_oversized_context(),
        );
        let (events, outcome) = match failed {
            Some(outcome) => {
                tracing::warn!(
                    outcome = ?outcome,
                    est_input_tokens = self.input_tokens,
                    "上游响应以错误结束，补发 error 事件"
                );
                if let Some(error) = outcome.usage_error() {
                    self.record_failed_usage(error);
                }
                (outcome.error_event().into_iter().collect(), outcome)
            }
            None => {
                let (events, outcome) = self.generate_final_events_raw();
                (self.render_outbound(events), outcome)
            }
        };
        if let Some(feedback) = &self.upstream_feedback {
            outcome.report(feedback);
        }
        events
    }

    /// 请求以错误结束、未走正常收尾时的 usage 记录（上游未计费时不记，见 [`should_record_usage`]）
    pub(crate) fn record_failed_usage(&self, error: UsageError) {
        if !should_record_usage(Some(error), self.metering_usage) {
            return;
        }
        let final_input_tokens =
            cap_input_tokens(self.input_tokens, self.input_tokens, &self.model);
        let billed_output_tokens = self.output_tokens();
        let billed_output = self.billed_output(billed_output_tokens);
        // 上游已计费即已处理过这段前缀：与正常收尾同口径结算
        let cache_read = self.settle_cache_read(final_input_tokens, &billed_output);
        self.record_usage(
            final_input_tokens,
            billed_output_tokens,
            cache_read.tokens,
            Some(error),
        );
    }

    /// 设置用量追踪
    pub fn with_usage_tracking(
        mut self,
        tracker: Option<Arc<UsageTracker>>,
        api_key_id: Option<u32>,
        credential_id: Option<u64>,
        client_ip: Option<String>,
        user_agent: Option<String>,
    ) -> Self {
        self.usage_tracker = tracker;
        self.api_key_id = api_key_id;
        self.credential_id = credential_id;
        self.client_ip = client_ip;
        self.user_agent = user_agent;
        self
    }

    /// 结算本次请求的 cache_read（客户端 usage 与用量日志同一口径，见 `crate::cache`）。
    ///
    /// 前缀观测会把本次前缀登记到账号上，每个请求只能在收尾时调用一次。
    fn settle_cache_read(
        &self,
        input_tokens: i32,
        billed_output: &crate::model::usage::BilledOutput,
    ) -> crate::cache::CacheRead {
        let prefix_read = self
            .prefix_probe
            .as_ref()
            .zip(self.credential_id)
            .map(|(probe, account)| probe.settle(account, input_tokens));
        let read = crate::cache::estimate_cache_read(
            &self.model,
            input_tokens,
            self.metering_cache_read_tokens,
            self.metering_usage,
            &self.replayed_reasoning,
            billed_output,
            prefix_read,
        );
        tracing::info!(
            model = %self.model,
            input_tokens,
            cache_read = read.tokens,
            source = ?read.source,
            prefix_read,
            "[cache] 本次缓存命中估算"
        );
        read
    }

    /// 生成 message_start 事件
    ///
    /// 缓存命中要等 credits 到达后才能估算：这里报完整输入、缓存为 0，终值由 message_delta 给出
    /// （Claude Code 用 message_delta 里大于 0 的字段覆盖 message_start）。
    pub fn create_message_start_event(&self) -> serde_json::Value {
        let input = cap_input_tokens(self.input_tokens, self.input_tokens, &self.model);
        json!({
            "type": "message_start",
            "message": {
                "id": self.message_id,
                "type": "message",
                "role": "assistant",
                "content": [],
                "model": self.model,
                "stop_reason": null,
                "stop_sequence": null,
                "usage": {
                    "input_tokens": scale_for_client(input, &self.model),
                    "output_tokens": 1,
                    "cache_creation_input_tokens": 0,
                    "cache_read_input_tokens": 0
                }
            }
        })
    }

    /// 生成初始事件序列（仅 message_start）
    ///
    /// 文本块等到实际收到内容时再创建：上游推理无论客户端是否开启 thinking 都会下发，
    /// 预建文本块会让 thinking 块排到一个空文本块之后。
    pub fn generate_initial_events(&mut self) -> Vec<SseEvent> {
        let msg_start = self.create_message_start_event();
        self.state_manager
            .handle_message_start(msg_start)
            .into_iter()
            .collect()
    }

    /// 处理 Kiro 事件并转换为 Anthropic SSE 事件
    pub fn process_kiro_event(&mut self, event: &Event) -> Vec<SseEvent> {
        let events = self.process_kiro_event_raw(event);
        self.render_outbound(events)
    }

    fn process_kiro_event_raw(&mut self, event: &Event) -> Vec<SseEvent> {
        match event {
            Event::AssistantResponse(resp) => self.process_assistant_response(&resp.content),
            Event::ReasoningContent(reasoning) => self.process_native_reasoning(reasoning),
            Event::ToolUse(tool_use) => self.process_tool_use(tool_use),
            Event::ContextUsage(context_usage) => {
                // contextUsage 本地化：仅保留事件接收用于 stop_reason 兜底判定，
                // 不再用 percentage × window 反算 input_tokens。
                // final_input_tokens 来源改为：metering.inputTokens 真值 → 本地 count_all_tokens。
                self.context_usage_percentage = Some(context_usage.context_usage_percentage);
                if context_usage.context_usage_percentage >= 100.0 {
                    self.state_manager
                        .set_stop_reason("model_context_window_exceeded");
                }
                tracing::debug!(
                    "[deprecated] contextUsageEvent: {:.2}% (仅记录, 不参与 input_tokens 反算)",
                    context_usage.context_usage_percentage,
                );
                Vec::new()
            }
            Event::Metering(metering) => {
                let prev = self.metering_usage;
                self.metering_usage = Some(metering.usage);
                self.metering_cache_read_tokens = metering.cache_read_input_tokens;
                if let Some(prev_val) = prev {
                    tracing::warn!(
                        "[metering] 同一请求收到第2次 meteringEvent: new={} {} prev={} model={}",
                        metering.usage,
                        metering.unit_plural,
                        prev_val,
                        self.model
                    );
                } else {
                    tracing::info!(
                        "[metering] meteringEvent: usage={} {} model={} cache_read={:?} cache_creation={:?}",
                        metering.usage,
                        metering.unit_plural,
                        self.model,
                        metering.cache_read_input_tokens,
                        metering.cache_creation_input_tokens
                    );
                }
                Vec::new()
            }

            Event::CodeReference(code_ref) => {
                for r in &code_ref.references {
                    tracing::debug!(
                        "[code_reference] license={} repo={} url={}",
                        r.license_name,
                        r.repository,
                        r.url
                    );
                }
                Vec::new()
            }
            Event::Metadata(metadata) => {
                // 异常类 stopReason 由 termination 判定，此处只映射正常收尾的 stop_reason
                match metadata.upstream_stop() {
                    Some(UpstreamStop::MaxTokens) => {
                        self.state_manager.set_stop_reason("max_tokens");
                    }
                    Some(UpstreamStop::ContextWindowExceeded) => {
                        self.state_manager
                            .set_stop_reason("model_context_window_exceeded");
                    }
                    _ => {}
                }
                Vec::new()
            }
            Event::Error {
                error_code,
                error_message,
            } => {
                tracing::error!("收到错误事件: {} - {}", error_code, error_message);
                Vec::new()
            }
            Event::Exception {
                exception_type,
                message,
            } => {
                // 处理 ContentLengthExceededException
                if exception_type == super::CONTENT_LENGTH_EXCEEDED_EXCEPTION {
                    self.state_manager.set_stop_reason("max_tokens");
                }
                tracing::warn!("收到异常事件: {} - {}", exception_type, message);
                Vec::new()
            }
            _ => Vec::new(),
        }
    }

    /// 处理助手响应事件
    pub(crate) fn process_assistant_response(&mut self, content: &str) -> Vec<SseEvent> {
        if content.is_empty() {
            return Vec::new();
        }

        // 响应前缀防护：剔除模型偶发回声用户 `<system-reminder>` 块的情况（见模块
        // 文档 `echo_guard`）。放在最前面统一处理：命中时整段剥离后再继续走正常
        // 流程（含 thinking 标签解析），不命中/已决出结果后永久透传，不影响吞吐。
        let content = self.echo_guard.filter(content);
        let content = content.as_str();
        if content.is_empty() {
            return Vec::new();
        }

        // 缓冲全文而非逐段编码 —— BPE 统一留到 output_tokens() 收尾做一次：
        // 跨 chunk 的 BPE 合并（如标签 `<th` + `inking>`）无法拆开分段累加
        self.output_full_text.push_str(content);

        let mut events = self.finish_native_thinking();
        // 如果启用了thinking，需要处理thinking块
        if self.thinking_enabled && !self.native_thinking_seen {
            events.extend(self.process_content_with_thinking(content));
            return events;
        }

        // 非 thinking 模式同样复用统一的 text_delta 发送逻辑，
        // 以便在 tool_use 自动关闭文本块后能够自愈重建新的文本块，避免“吞字”。
        events.extend(self.create_text_delta_events(content));
        events
    }

    /// 处理包含thinking块的内容
    fn process_content_with_thinking(&mut self, content: &str) -> Vec<SseEvent> {
        let mut events = Vec::new();

        // 将内容添加到缓冲区进行处理
        self.thinking_buffer.push_str(content);

        loop {
            if !self.in_thinking_block && !self.thinking_extracted {
                // 查找 <thinking> 开始标签（跳过被反引号包裹的）
                if let Some(start_pos) = find_real_thinking_start_tag(&self.thinking_buffer) {
                    // 发送 <thinking> 之前的内容作为 text_delta
                    // 注意：如果前面只是空白字符（如 adaptive 模式返回的 \n\n），则跳过，
                    // 避免在 thinking 块之前产生无意义的 text 块导致客户端解析失败
                    let before_thinking = self.thinking_buffer[..start_pos].to_string();
                    if !before_thinking.is_empty() && !before_thinking.trim().is_empty() {
                        events.extend(self.create_text_delta_events(&before_thinking));
                    }

                    // 进入 thinking 块
                    self.in_thinking_block = true;
                    self.strip_thinking_leading_newline = true;
                    self.thinking_buffer =
                        self.thinking_buffer[start_pos + "<thinking>".len()..].to_string();

                    // 创建 thinking 块的 content_block_start 事件
                    let thinking_index = self.state_manager.next_block_index();
                    self.thinking_block_index = Some(thinking_index);
                    let start_events = self.state_manager.handle_content_block_start(
                        thinking_index,
                        "thinking",
                        json!({
                            "type": "content_block_start",
                            "index": thinking_index,
                            "content_block": {
                                "type": "thinking",
                                "thinking": ""
                            }
                        }),
                    );
                    events.extend(start_events);
                } else {
                    // 没有找到 <thinking>，检查是否可能是部分标签
                    // 保留可能是部分标签的内容
                    let target_len = self
                        .thinking_buffer
                        .len()
                        .saturating_sub("<thinking>".len());
                    let safe_len = find_char_boundary(&self.thinking_buffer, target_len);
                    if safe_len > 0 {
                        let safe_content = self.thinking_buffer[..safe_len].to_string();
                        // 如果 thinking 尚未提取，且安全内容只是空白字符，
                        // 则不发送为 text_delta，继续保留在缓冲区等待更多内容。
                        // 这避免了 4.6 模型中 <thinking> 标签跨事件分割时，
                        // 前导空白（如 "\n\n"）被错误地创建为 text 块，
                        // 导致 text 块先于 thinking 块出现的问题。
                        if !safe_content.is_empty() && !safe_content.trim().is_empty() {
                            events.extend(self.create_text_delta_events(&safe_content));
                            self.thinking_buffer = self.thinking_buffer[safe_len..].to_string();
                        }
                    }
                    break;
                }
            } else if self.in_thinking_block {
                // 剥离 <thinking> 标签后紧跟的换行符（可能跨 chunk）
                if self.strip_thinking_leading_newline {
                    if self.thinking_buffer.starts_with('\n') {
                        self.thinking_buffer = self.thinking_buffer[1..].to_string();
                        self.strip_thinking_leading_newline = false;
                    } else if !self.thinking_buffer.is_empty() {
                        // buffer 非空但不以 \n 开头，不再需要剥离
                        self.strip_thinking_leading_newline = false;
                    }
                    // buffer 为空时保留标志，等待下一个 chunk
                }

                // 在 thinking 块内，查找 </thinking> 结束标签（跳过被反引号包裹的）
                if let Some(end_pos) = find_real_thinking_end_tag(&self.thinking_buffer) {
                    // 提取 thinking 内容
                    let thinking_content = self.thinking_buffer[..end_pos].to_string();
                    if !thinking_content.is_empty()
                        && let Some(thinking_index) = self.thinking_block_index
                    {
                        events.push(
                            self.create_thinking_delta_event(thinking_index, &thinking_content),
                        );
                    }

                    // 结束 thinking 块
                    self.in_thinking_block = false;
                    self.thinking_extracted = true;

                    // 发送空的 thinking_delta 事件，然后发送 content_block_stop 事件
                    if let Some(thinking_index) = self.thinking_block_index {
                        // 先发送空的 thinking_delta
                        events.push(self.create_thinking_delta_event(thinking_index, ""));
                        // 注入 signature_delta（必须在 content_block_stop 之前）
                        events.extend(self.take_signature_events());
                        // 再发送 content_block_stop
                        if let Some(stop_event) =
                            self.state_manager.handle_content_block_stop(thinking_index)
                        {
                            events.push(stop_event);
                        }
                    }

                    // 剥离 `</thinking>\n\n`（find_real_thinking_end_tag 已确认 \n\n 存在）
                    self.thinking_buffer =
                        self.thinking_buffer[end_pos + "</thinking>\n\n".len()..].to_string();
                } else {
                    // 没有找到结束标签，发送当前缓冲区内容作为 thinking_delta。
                    // 保留末尾可能是部分 `</thinking>\n\n` 的内容：
                    // find_real_thinking_end_tag 要求标签后有 `\n\n` 才返回 Some，
                    // 因此保留区必须覆盖 `</thinking>\n\n` 的完整长度（13 字节），
                    // 否则当 `</thinking>` 已在 buffer 但 `\n\n` 尚未到达时，
                    // 标签的前几个字符会被错误地作为 thinking_delta 发出。
                    let target_len = self
                        .thinking_buffer
                        .len()
                        .saturating_sub("</thinking>\n\n".len());
                    let safe_len = find_char_boundary(&self.thinking_buffer, target_len);
                    if safe_len > 0 {
                        let safe_content = self.thinking_buffer[..safe_len].to_string();
                        if !safe_content.is_empty()
                            && let Some(thinking_index) = self.thinking_block_index
                        {
                            events.push(
                                self.create_thinking_delta_event(thinking_index, &safe_content),
                            );
                        }
                        self.thinking_buffer = self.thinking_buffer[safe_len..].to_string();
                    }
                    break;
                }
            } else {
                // thinking 已提取完成，剩余内容作为 text_delta
                if !self.thinking_buffer.is_empty() {
                    let remaining = self.thinking_buffer.clone();
                    self.thinking_buffer.clear();
                    events.extend(self.create_text_delta_events(&remaining));
                }
                break;
            }
        }

        events
    }

    /// 创建 text_delta 事件
    ///
    /// 如果文本块尚未创建，会先创建文本块。
    /// 当发生 tool_use 时，状态机会自动关闭当前文本块；后续文本会自动创建新的文本块继续输出。
    ///
    /// 返回值包含可能的 content_block_start 事件和 content_block_delta 事件。
    fn create_text_delta_events(&mut self, text: &str) -> Vec<SseEvent> {
        let mut events = Vec::new();

        // 如果当前 text_block_index 指向的块已经被关闭（例如 tool_use 开始时自动 stop），
        // 则丢弃该索引并创建新的文本块继续输出，避免 delta 被状态机拒绝导致“吞字”。
        if let Some(idx) = self.text_block_index
            && !self.state_manager.is_block_open_of_type(idx, "text")
        {
            self.text_block_index = None;
        }

        // 获取或创建文本块索引
        let text_index = if let Some(idx) = self.text_block_index {
            idx
        } else {
            // 文本块尚未创建，需要先创建
            let idx = self.state_manager.next_block_index();
            self.text_block_index = Some(idx);

            // 发送 content_block_start 事件
            let start_events = self.state_manager.handle_content_block_start(
                idx,
                "text",
                json!({
                    "type": "content_block_start",
                    "index": idx,
                    "content_block": {
                        "type": "text",
                        "text": ""
                    }
                }),
            );
            events.extend(start_events);
            idx
        };

        // 发送 content_block_delta 事件
        if let Some(delta_event) = self.state_manager.handle_content_block_delta(
            text_index,
            json!({
                "type": "content_block_delta",
                "index": text_index,
                "delta": {
                    "type": "text_delta",
                    "text": text
                }
            }),
        ) {
            // 只统计真正发出的文本 —— 被状态机拒绝的 delta 客户端收不到，不应计入上报
            self.visible_full_text.push_str(text);
            events.push(delta_event);
        }

        events
    }

    /// 客户端可见 thinking 文案入缓存（落库 thinking_token_count 统计口径）；
    /// 空串跳过（content_block_stop 前的收尾空事件不计数）。
    /// 流式两条发射路径（标签解析 / native reasoning）统一经此收敛，防第三发射点遗漏。
    pub(super) fn accumulate_thinking(&mut self, text: &str) {
        if !text.is_empty() {
            self.thinking_full_text.push_str(text);
        }
    }

    /// 创建 thinking_delta 事件
    ///
    /// 非空内容同时写入 thinking 全文缓存（落库为 thinking_token_count 的统计口径：
    /// 客户端实际收到的 thinking 文案 BPE 真值，与 visible_full_text 同语义）；空串事件（
    /// content_block_stop 前的收尾）不计数。
    fn create_thinking_delta_event(&mut self, index: i32, thinking: &str) -> SseEvent {
        self.accumulate_thinking(thinking);
        SseEvent::new(
            "content_block_delta",
            json!({
                "type": "content_block_delta",
                "index": index,
                "delta": {
                    "type": "thinking_delta",
                    "thinking": thinking
                }
            }),
        )
    }

    /// 处理工具使用事件
    pub(crate) fn process_tool_use(
        &mut self,
        tool_use: &crate::kiro::model::events::ToolUseEvent,
    ) -> Vec<SseEvent> {
        let mut events = self.finish_native_thinking();

        self.state_manager.set_has_tool_use(true);

        // tool_use 必须发生在 thinking 结束之后。
        // 但当 `</thinking>` 后面没有 `\n\n`（例如紧跟 tool_use 或流结束）时，
        // thinking 结束标签会滞留在 thinking_buffer，导致后续 flush 时把 `</thinking>` 当作内容输出。
        // 这里在开始 tool_use block 前做一次“边界场景”的结束标签识别与过滤。
        if self.thinking_enabled
            && self.in_thinking_block
            && let Some(end_pos) = find_real_thinking_end_tag_at_buffer_end(&self.thinking_buffer)
        {
            let thinking_content = self.thinking_buffer[..end_pos].to_string();
            if !thinking_content.is_empty()
                && let Some(thinking_index) = self.thinking_block_index
            {
                events.push(self.create_thinking_delta_event(thinking_index, &thinking_content));
            }

            // 结束 thinking 块
            self.in_thinking_block = false;
            self.thinking_extracted = true;

            if let Some(thinking_index) = self.thinking_block_index {
                // 先发送空的 thinking_delta
                events.push(self.create_thinking_delta_event(thinking_index, ""));
                // 注入 signature_delta（必须在 content_block_stop 之前）
                events.extend(self.take_signature_events());
                // 再发送 content_block_stop
                if let Some(stop_event) =
                    self.state_manager.handle_content_block_stop(thinking_index)
                {
                    events.push(stop_event);
                }
            }

            // 把结束标签后的内容当作普通文本（通常为空或空白）
            let after_pos = end_pos + "</thinking>".len();
            let remaining = self.thinking_buffer[after_pos..].trim_start().to_string();
            self.thinking_buffer.clear();
            if !remaining.is_empty() {
                events.extend(self.create_text_delta_events(&remaining));
            }
        }

        // thinking 模式下，process_content_with_thinking 可能会为了探测 `<thinking>` 而暂存一小段尾部文本。
        // 如果此时直接开始 tool_use，状态机会自动关闭 text block，导致这段"待输出文本"看起来被 tool_use 吞掉。
        // 约束：只在尚未进入 thinking block、且 thinking 尚未被提取时，将缓冲区当作普通文本 flush。
        if self.thinking_enabled
            && !self.in_thinking_block
            && !self.thinking_extracted
            && !self.thinking_buffer.is_empty()
        {
            let buffered = std::mem::take(&mut self.thinking_buffer);
            events.extend(self.create_text_delta_events(&buffered));
        }

        // 获取或分配块索引
        let block_index = if let Some(&idx) = self.tool_block_indices.get(&tool_use.tool_use_id) {
            idx
        } else {
            let idx = self.state_manager.next_block_index();
            self.tool_block_indices
                .insert(tool_use.tool_use_id.clone(), idx);
            idx
        };

        // 发送 content_block_start
        let start_events = self.state_manager.handle_content_block_start(
            block_index,
            "tool_use",
            json!({
                "type": "content_block_start",
                "index": block_index,
                "content_block": {
                    "type": "tool_use",
                    "id": tool_use.tool_use_id,
                    "name": tool_use.name,
                    "input": {}
                }
            }),
        );
        events.extend(start_events);

        // 发送参数增量 (ToolUseEvent.input 是 String 类型)
        if !tool_use.input.is_empty() {
            // 计费口径与 text 分支一致（`process_assistant_response` 也是无条件累加）：
            // 上游已生成这段内容即已计费，代理侧状态机是否转发不改变上游成本。
            self.output_full_text.push_str(&tool_use.input);

            if let Some(delta_event) = self.state_manager.handle_content_block_delta(
                block_index,
                json!({
                    "type": "content_block_delta",
                    "index": block_index,
                    "delta": {
                        "type": "input_json_delta",
                        "partial_json": tool_use.input
                    }
                }),
            ) {
                // 上报口径只统计真正发出的内容 —— 被状态机拒绝的 delta 客户端收不到
                // （如同一 tool_use_id 在 stop 之后又收到迟到帧）。
                self.visible_full_text.push_str(&tool_use.input);
                events.push(delta_event);
            }
        }

        // 如果是完整的工具调用（stop=true），发送 content_block_stop
        if tool_use.stop
            && let Some(stop_event) = self.state_manager.handle_content_block_stop(block_index)
        {
            events.push(stop_event);
        }

        events
    }

    /// 生成最终事件序列
    #[cfg(test)]
    pub fn generate_final_events(&mut self) -> Vec<SseEvent> {
        let (events, _) = self.generate_final_events_raw();
        self.render_outbound(events)
    }

    /// 组装收尾事件并给出结局：Completed，或只有 thinking 就被截断（ThinkingOnlyTruncated）
    fn generate_final_events_raw(&mut self) -> (Vec<SseEvent>, ResponseOutcome) {
        let mut events = self.finish_native_thinking();

        // Flush thinking_buffer 中的剩余内容
        if self.thinking_enabled && !self.thinking_buffer.is_empty() {
            if self.in_thinking_block {
                // 末尾可能残留 `</thinking>`（例如紧跟 tool_use 或流结束），需要在 flush 时过滤掉结束标签。
                if let Some(end_pos) =
                    find_real_thinking_end_tag_at_buffer_end(&self.thinking_buffer)
                {
                    let thinking_content = self.thinking_buffer[..end_pos].to_string();
                    if !thinking_content.is_empty()
                        && let Some(thinking_index) = self.thinking_block_index
                    {
                        events.push(
                            self.create_thinking_delta_event(thinking_index, &thinking_content),
                        );
                    }

                    // 关闭 thinking 块：先发送空的 thinking_delta，再发送 content_block_stop
                    if let Some(thinking_index) = self.thinking_block_index {
                        events.push(self.create_thinking_delta_event(thinking_index, ""));
                        // 注入 signature_delta（必须在 content_block_stop 之前）
                        events.extend(self.take_signature_events());
                        if let Some(stop_event) =
                            self.state_manager.handle_content_block_stop(thinking_index)
                        {
                            events.push(stop_event);
                        }
                    }

                    // 把结束标签后的内容当作普通文本（通常为空或空白）
                    let after_pos = end_pos + "</thinking>".len();
                    let remaining = self.thinking_buffer[after_pos..].trim_start().to_string();
                    self.thinking_buffer.clear();
                    self.in_thinking_block = false;
                    self.thinking_extracted = true;
                    if !remaining.is_empty() {
                        events.extend(self.create_text_delta_events(&remaining));
                    }
                } else {
                    // 如果还在 thinking 块内，发送剩余内容作为 thinking_delta
                    // （mem::take 解借用冲突：参数与 &mut self 同源；该分支块末
                    // self.thinking_buffer.clear() 无条件清空，take 后无需还回）
                    if let Some(thinking_index) = self.thinking_block_index {
                        let buffer = std::mem::take(&mut self.thinking_buffer);
                        events.push(self.create_thinking_delta_event(thinking_index, &buffer));
                    }
                    // 关闭 thinking 块：先发送空的 thinking_delta，再发送 content_block_stop
                    if let Some(thinking_index) = self.thinking_block_index {
                        // 先发送空的 thinking_delta
                        events.push(self.create_thinking_delta_event(thinking_index, ""));
                        // 注入 signature_delta（必须在 content_block_stop 之前）
                        events.extend(self.take_signature_events());
                        // 再发送 content_block_stop
                        if let Some(stop_event) =
                            self.state_manager.handle_content_block_stop(thinking_index)
                        {
                            events.push(stop_event);
                        }
                    }
                }
            } else {
                // 否则发送剩余内容作为 text_delta
                let buffer_content = self.thinking_buffer.clone();
                events.extend(self.create_text_delta_events(&buffer_content));
            }
            self.thinking_buffer.clear();
        }

        // 整个流只产生了 thinking 块，没有 text 也没有 tool_use：
        // - 上游给过收尾理由（MAX_TOKENS / 上下文超限）：预算确实用完，按该理由正常收尾，
        //   补一个空格 text 块，确保 content 数组中有 text 块
        // - 上游没给理由就结束：回复被截断，记账后改发可重试错误（见本函数末尾）
        let thinking_only = (self.thinking_block_index.is_some() || self.native_thinking_seen)
            && !self.state_manager.has_non_thinking_blocks();
        let truncated_after_thinking = thinking_only && !self.state_manager.has_stop_reason();
        if thinking_only && !truncated_after_thinking {
            events.extend(self.create_text_delta_events(" "));
        }
        let outcome = if truncated_after_thinking {
            ResponseOutcome::ThinkingOnlyTruncated
        } else {
            ResponseOutcome::Completed
        };

        // contextUsage 本地化：input_tokens 来源改为 metering 真值 → 本地估算（self.input_tokens）
        // self.context_input_tokens 已弃用（始终为 None），保留诊断字段
        let raw_final_input_tokens = self.input_tokens;
        let final_input_tokens =
            cap_input_tokens(raw_final_input_tokens, self.input_tokens, &self.model);

        // 本地估算 ≥ 1M 兜底触发 stop_reason
        if final_input_tokens >= 1_000_000 {
            self.state_manager
                .set_stop_reason("model_context_window_exceeded");
        }

        tracing::info!(
            "[input_tokens] 本地化: estimated={} final={}",
            self.input_tokens,
            final_input_tokens
        );

        // 计费口径（含 thinking），派生一次复用
        let billed_output_tokens = self.output_tokens();
        // 对客户端上报：默认「可见输出」（已排除 thinking）；outputTokensIncludeThinking
        // 开启时与官方口径一致，上报含 thinking 的计费值
        let reported_output_tokens = if crate::anthropic::stream::output_tokens_include_thinking() {
            billed_output_tokens
        } else {
            self.visible_output_tokens()
        };

        // 客户端 message_delta 与用量日志同一个 cache_read
        let billed_output = self.billed_output(billed_output_tokens);
        let cache_read = self
            .settle_cache_read(final_input_tokens, &billed_output)
            .tokens;
        self.record_usage(
            final_input_tokens,
            billed_output_tokens,
            cache_read,
            outcome.usage_error(),
        );

        if let Some(error_event) = outcome.error_event() {
            tracing::warn!(
                model = %self.model,
                est_input_tokens = final_input_tokens,
                "上游在 thinking 之后无理由结束（无正文、无工具调用），补发 error 事件"
            );
            events.extend(self.state_manager.generate_error_final_events(error_event));
            return (events, outcome);
        }

        events.extend(self.state_manager.generate_final_events(
            final_input_tokens - cache_read,
            reported_output_tokens,
            Some(0),
            Some(cache_read),
            self.context_usage_percentage,
            Some(0),
            Some(0),
            &self.model,
        ));

        (events, outcome)
    }

    /// 写入 usage 记录（`error` 为 Some 时仅在上游计费后才记，见 [`should_record_usage`]）
    fn record_usage(
        &self,
        final_input_tokens: i32,
        billed_output_tokens: i32,
        cache_read: i32,
        error: Option<UsageError>,
    ) {
        if !should_record_usage(error, self.metering_usage) {
            return;
        }
        if let (Some(tracker), Some(key_id)) = (&self.usage_tracker, self.api_key_id) {
            let credits_per_ktok = self.metering_usage.map(|c| {
                if final_input_tokens > 0 {
                    c / (final_input_tokens as f64) * 1000.0
                } else {
                    0.0
                }
            });
            let effective_rate = self.metering_usage.map(|c| {
                let denom = final_input_tokens as f64 + 5.0 * billed_output_tokens as f64;
                if denom > 0.0 { c / denom * 1000.0 } else { 0.0 }
            });
            tracing::info!(
                "[usage] 入库: model={} input={} output={} metering_credits={:?} credits_per_ktok={:?} effective_rate={:?} cache_read={} api_key={} credential={:?}",
                self.model,
                final_input_tokens,
                billed_output_tokens,
                self.metering_usage,
                credits_per_ktok,
                effective_rate,
                cache_read,
                key_id,
                self.credential_id
            );
            // 计时口径：first_token_ms = 起点 → 首个客户端可见 SSE 产出（空输出帧
            // 与 keepalive 不计）；duration_ms = 起点 → 落库前。
            // 起点未注入（None）时二者均不计（等价旧数据）
            let (first_token_ms, duration_ms) = match self.request_started_at {
                Some(started) => (
                    self.first_token_at
                        .map(|t| (t - started).as_millis() as u64),
                    Some((Instant::now() - started).as_millis() as u64),
                ),
                None => (None, None),
            };
            // 无 thinking 时落 None，与「None = 未产生思考」语义统一（非流式同口径）
            let thinking_tokens = self.thinking_token_count();
            tracker.record(
                key_id,
                self.model.clone(),
                final_input_tokens,
                billed_output_tokens,
                crate::model::usage::RecordOptions {
                    credential_id: self.credential_id,
                    client_ip: self.client_ip.clone(),
                    credits_used: self.metering_usage,
                    cache_read_input_tokens: Some(cache_read),
                    cache_creation_input_tokens: Some(0),
                    effort: self.effort.clone(),
                    thinking_budget_tokens: self.thinking_budget_tokens,
                    first_token_ms,
                    duration_ms,
                    thinking_token_count: (thinking_tokens > 0).then_some(thinking_tokens),
                    is_stream: Some(true),
                    user_agent: self.user_agent.clone(),
                    thinking_loss: self.thinking_loss,
                    api_format: self.api_format,
                    error,
                },
            );
        }
    }
}
