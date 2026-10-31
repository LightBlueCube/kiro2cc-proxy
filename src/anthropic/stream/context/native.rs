//! Kiro 原生推理事件到 Anthropic thinking 块的转换：native reasoning 流的组装与收尾事件生成。

use serde_json::json;

use super::StreamContext;
use crate::anthropic::stream::state::SseEvent;
use crate::kiro::model::events::ReasoningContentEvent;

impl StreamContext {
    pub(super) fn process_native_reasoning(
        &mut self,
        reasoning: &ReasoningContentEvent,
    ) -> Vec<SseEvent> {
        tracing::debug!(
            model = %self.model,
            message_id = %self.message_id,
            text_chars = reasoning.text.chars().count(),
            signature_bytes = reasoning.signature.len(),
            thinking_enabled = self.thinking_enabled,
            "[native-thinking] received reasoningContentEvent"
        );
        // 计费口径：缓冲全文，收尾 BPE 一次编码（见 output_tokens）
        self.output_full_text.push_str(&reasoning.text);
        // 反推口径：签名收尾一个推理块，可能分多个事件下发；紧随签名的纯签名事件属同一块
        self.native_reasoning_full_text.push_str(&reasoning.text);
        let has_signature = !reasoning.signature.is_empty();
        if has_signature {
            self.native_signature_chars += reasoning.signature.len();
            let continues_signature = self.in_native_signature_run && reasoning.text.is_empty();
            if !continues_signature {
                self.native_signature_blocks += 1;
            }
        }
        self.in_native_signature_run = has_signature;
        // 上游推理原样下发，与客户端是否开启 thinking 无关
        if reasoning.text.is_empty() && reasoning.signature.is_empty() {
            return Vec::new();
        }

        let mut events = Vec::new();
        let index = match self.native_thinking_index {
            Some(index) => index,
            None => {
                if !self.in_thinking_block && !self.thinking_buffer.is_empty() {
                    let pending = std::mem::take(&mut self.thinking_buffer);
                    events.extend(self.create_text_delta_events(&pending));
                }
                if let Some(index) = self.text_block_index.take()
                    && let Some(stop) = self.state_manager.handle_content_block_stop(index)
                {
                    events.push(stop);
                }
                let index = self.state_manager.next_block_index();
                events.extend(self.state_manager.handle_content_block_start(
                    index,
                    "thinking",
                    json!({
                        "type": "content_block_start",
                        "index": index,
                        "content_block": {"type": "thinking", "thinking": ""}
                    }),
                ));
                self.native_thinking_index = Some(index);
                self.native_thinking_seen = true;
                index
            }
        };

        for (kind, field, value) in [
            ("thinking_delta", "thinking", &reasoning.text),
            ("signature_delta", "signature", &reasoning.signature),
        ] {
            if !value.is_empty()
                && let Some(delta) = self.state_manager.handle_content_block_delta(
                    index,
                    json!({
                        "type": "content_block_delta",
                        "index": index,
                        "delta": {"type": kind, (field): value}
                    }),
                )
            {
                // 仅统计被状态机接受、客户端实际收到的 thinking 文案
                if kind == "thinking_delta" {
                    self.accumulate_thinking(value);
                }
                tracing::debug!(
                    model = %self.model,
                    message_id = %self.message_id,
                    block_index = index,
                    delta_type = kind,
                    "[native-thinking] queued SSE delta"
                );
                events.push(delta);
            }
        }
        events
    }

    /// 正文与工具边界共用；重复收尾不产生额外事件。
    pub(crate) fn finish_native_thinking(&mut self) -> Vec<SseEvent> {
        self.native_thinking_index
            .take()
            .and_then(|index| self.state_manager.handle_content_block_stop(index))
            .into_iter()
            .collect()
    }
}
