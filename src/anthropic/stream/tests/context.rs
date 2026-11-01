// Copyright (c) 2026 Harllan He. Licensed under MIT.
// 空响应检测与上下文窗口测试（自 stream/tests.rs 拆出，纯代码搬移）
#[cfg(test)]
mod tests {
    use crate::anthropic::stream::{StreamContext, context_window_for_model};

    /// 捕获 DEBUG 及以上 tracing 输出（同步闭包），返回 (闭包返回值, 日志文本)
    fn capture_debug_logs<T>(f: impl FnOnce() -> T) -> (T, String) {
        use std::io::Write;
        use std::sync::{Arc, Mutex};

        #[derive(Clone, Default)]
        struct BufWriter(Arc<Mutex<Vec<u8>>>);
        impl Write for BufWriter {
            fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
                self.0.lock().unwrap().extend_from_slice(buf);
                Ok(buf.len())
            }
            fn flush(&mut self) -> std::io::Result<()> {
                Ok(())
            }
        }

        let buf = BufWriter::default();
        let writer = buf.clone();
        let subscriber = tracing_subscriber::fmt()
            .with_max_level(tracing::Level::DEBUG)
            .with_ansi(false)
            .with_writer(move || writer.clone())
            .finish();
        let result = tracing::subscriber::with_default(subscriber, f);
        (
            result,
            String::from_utf8(buf.0.lock().unwrap().clone()).unwrap(),
        )
    }

    #[test]
    fn test_empty_response_detected() {
        // 上游完全没发任何内容事件 → is_empty_response 为 true
        let mut ctx = StreamContext::new_with_thinking("test-model", 1, false);
        let _ = ctx.generate_initial_events();
        assert!(
            ctx.is_empty_response(),
            "未收到任何内容事件时应判定为空响应"
        );
    }

    #[test]
    fn test_empty_response_oversized_context_by_threshold() {
        // contextUsage 本地化后所有模型按 1M 窗口，阈值 = 1M * 0.28 = 280_000
        // 大输入(>=28万)的空响应 → 判定为上下文过大
        let big = StreamContext::new_with_thinking("test-model", 300_000, true);
        assert!(
            big.empty_response_is_oversized_context(),
            "input 30万应判定为上下文过大"
        );
        // 小输入的空响应 → 视为偶发，可重试
        let small = StreamContext::new_with_thinking("test-model", 100_000, true);
        assert!(
            !small.empty_response_is_oversized_context(),
            "input 10万不应判定为上下文过大"
        );
    }

    #[test]
    fn test_non_empty_response_not_flagged() {
        // 收到了文本内容 → 不应判定为空响应
        let mut ctx = StreamContext::new_with_thinking("test-model", 1, false);
        let _ = ctx.generate_initial_events();
        ctx.process_assistant_response("hello");
        assert!(!ctx.is_empty_response(), "已产生文本内容时不应判定为空响应");
    }

    #[test]
    fn test_tool_only_response_not_flagged() {
        // 只有工具调用、无文本 → 不是空响应
        let mut ctx = StreamContext::new_with_thinking("test-model", 1, false);
        let _ = ctx.generate_initial_events();
        ctx.process_tool_use(&crate::kiro::model::events::ToolUseEvent {
            name: "Bash".to_string(),
            tool_use_id: "toolu_1".to_string(),
            input: "{}".to_string(),
            stop: true,
        });
        assert!(!ctx.is_empty_response(), "仅有工具调用时不应判定为空响应");
    }

    #[test]
    fn test_near_empty_response_oversized_context_flagged() {
        // 大上下文（>28万）+ 极短输出（< 30 tokens）+ 无工具调用 → 视为退化空响应
        let mut ctx = StreamContext::new_with_thinking("test-model", 300_000, false);
        let _ = ctx.generate_initial_events();
        // 模拟极短输出（40 个 'a'，BPE 8 字符合并 1 token = 5 token）
        ctx.output_full_text = "a".repeat(40);
        assert!(
            ctx.is_empty_response(),
            "大上下文+极短输出应判定为近似空响应"
        );
    }

    #[test]
    fn test_near_empty_response_small_context_not_flagged() {
        // 小上下文 + 极短输出 → 不应判定为空响应（可能是正常的短回复）
        let mut ctx = StreamContext::new_with_thinking("test-model", 50_000, false);
        let _ = ctx.generate_initial_events();
        ctx.output_full_text = "a".repeat(40);
        assert!(
            !ctx.is_empty_response(),
            "小上下文+极短输出不应判定为空响应"
        );
    }

    #[test]
    fn test_near_empty_response_with_tool_use_not_flagged() {
        // 大上下文 + 极短输出 + 有工具调用 → 不应判定为空响应（工具调用是有效响应）
        let mut ctx = StreamContext::new_with_thinking("test-model", 300_000, false);
        let _ = ctx.generate_initial_events();
        ctx.output_full_text = "a".repeat(40);
        ctx.state_manager.set_has_tool_use(true);
        assert!(!ctx.is_empty_response(), "有工具调用时不应判定为空响应");
    }

    #[test]
    fn test_near_empty_long_output_not_flagged_at_byte_boundary() {
        // 3841 字节 > 30 × 128：在 cl100k 单 token 最长 128 字节的前提下必然 ≥ 31 token；
        // 本用例校验短路结论与全文编码一致（等价性保护），不单独证明短路分支本身
        let mut ctx = StreamContext::new_with_thinking("test-model", 300_000, false);
        let _ = ctx.generate_initial_events();
        ctx.output_full_text = "a".repeat(3841);
        assert!(!ctx.is_empty_response());
    }

    #[test]
    fn test_context_window_opus_4_6_is_1m() {
        assert_eq!(context_window_for_model("claude-opus-4-6"), 1_000_000);
        assert_eq!(
            context_window_for_model("claude-opus-4-6-thinking"),
            1_000_000
        );
    }

    #[test]
    fn test_context_window_fable_5_is_1m() {
        assert_eq!(context_window_for_model("claude-fable-5"), 1_000_000);
        assert_eq!(
            context_window_for_model("claude-fable-5-thinking"),
            1_000_000
        );
    }

    #[test]
    fn test_context_window_all_models_unified_to_1m() {
        // contextUsage 本地化后所有模型统一返回 1M
        assert_eq!(
            context_window_for_model("claude-haiku-4-5-20251001"),
            1_000_000
        );
        assert_eq!(context_window_for_model("unknown-model"), 1_000_000);
        assert_eq!(context_window_for_model(""), 1_000_000);
    }

    #[test]
    fn test_context_window_existing_branches_unchanged() {
        // 回归：4-7 / 4-8 / sonnet-4-6 仍为 1M
        assert_eq!(context_window_for_model("claude-opus-4-7"), 1_000_000);
        assert_eq!(context_window_for_model("claude-opus-4-8"), 1_000_000);
        assert_eq!(context_window_for_model("claude-sonnet-4-6"), 1_000_000);
    }

    #[test]
    fn test_first_token_marked_on_business_event() {
        // 首个客户端可见产出（AssistantResponse 非空 content）应记录首字时刻
        // 埋点在 stream.rs 按「本帧是否产出非空 SSE 事件」判定后调用
        let mut ctx = StreamContext::new_with_thinking("test-model", 10, false)
            .with_request_started_at(std::time::Instant::now());
        let inner = serde_json::from_str::<crate::kiro::model::events::AssistantResponseEvent>(
            r#"{"content":"hi"}"#,
        )
        .unwrap();
        let event = crate::kiro::model::events::Event::AssistantResponse(inner);
        ctx.mark_first_token(); // 模拟 stream.rs 在非空 SSE 产出处的埋点调用
        ctx.process_kiro_event(&event);
        assert!(ctx.first_token_at.is_some(), "非空产出应计首字");
    }

    #[test]
    fn test_first_token_not_marked_by_non_business_events() {
        // meteringEvent 产出空 SSE 事件，不计首字（stream.rs 埋点按产出非空判定，
        // 元信息帧 / 空输出帧天然产出空事件）；keepalive 在 SSE 层合成，同样不经过埋点
        let mut ctx = StreamContext::new_with_thinking("test-model", 10, false)
            .with_request_started_at(std::time::Instant::now());
        let event = crate::kiro::model::events::Event::Metering(
            crate::kiro::model::events::MeteringEvent {
                usage: 0.5,
                unit_plural: "credits".to_string(),
                unit: String::new(),
                cache_read_input_tokens: None,
                cache_creation_input_tokens: None,
            },
        );
        assert!(
            ctx.process_kiro_event(&event).is_empty(),
            "metering 帧产出应为空，锁定空产出不计首字的语义"
        );
        assert!(ctx.first_token_at.is_none(), "空产出不应计首字");
    }

    #[test]
    fn test_first_token_mark_is_idempotent() {
        // 多次 mark 仅首次生效
        let mut ctx = StreamContext::new_with_thinking("test-model", 10, false)
            .with_request_started_at(std::time::Instant::now());
        ctx.mark_first_token();
        let first = ctx.first_token_at;
        ctx.mark_first_token();
        assert_eq!(ctx.first_token_at, first);
    }

    #[tokio::test]
    async fn test_record_timing_none_without_started_at() {
        // 未注入起点（旧调用路径）时落库的 first_token_ms / duration_ms 均为 None
        let dir = std::env::temp_dir().join(format!("kiro2cc_ctx_none_{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("api_key_usage.json");
        let tracker = std::sync::Arc::new(crate::model::usage::UsageTracker::load(&path).unwrap());
        let mut ctx = StreamContext::new_with_thinking("test-model", 10, false)
            .with_usage_tracking(
                Some(std::sync::Arc::clone(&tracker)),
                Some(1),
                None,
                None,
                None,
            );
        ctx.mark_first_token();
        let _ = ctx.generate_final_events();
        let page =
            tracker.get_records_paged(1, 1, 10, &crate::model::usage::RecordLabels::default());
        assert_eq!(page.records.len(), 1);
        assert!(page.records[0].first_token_ms.is_none());
        assert!(page.records[0].duration_ms.is_none());
        let _ = std::fs::remove_dir_all(&dir);
    }

    fn metering(credits: f64) -> crate::kiro::model::events::Event {
        crate::kiro::model::events::Event::Metering(crate::kiro::model::events::MeteringEvent {
            usage: credits,
            unit_plural: "credits".to_string(),
            unit: String::new(),
            cache_read_input_tokens: None,
            cache_creation_input_tokens: None,
        })
    }

    #[tokio::test]
    async fn test_eof_empty_response_errors_and_records_usage_only_when_billed() {
        use crate::model::usage::{RecordLabels, UsageError, UsageTracker};

        let cases = [
            (1, "overloaded_error", UsageError::Empty),
            (
                300_000,
                "invalid_request_error",
                UsageError::EmptyOversizedContext,
            ),
        ];
        for (input_tokens, error_type, usage_error) in cases {
            for billed in [false, true] {
                let dir =
                    std::env::temp_dir().join(format!("kiro2cc_ctx_eof_{}", uuid::Uuid::new_v4()));
                std::fs::create_dir_all(&dir).unwrap();
                let tracker =
                    std::sync::Arc::new(UsageTracker::load(&dir.join("usage.json")).unwrap());
                let mut ctx = StreamContext::new_with_thinking("test-model", input_tokens, false)
                    .with_usage_tracking(
                        Some(std::sync::Arc::clone(&tracker)),
                        Some(1),
                        Some(7),
                        None,
                        None,
                    );
                let _ = ctx.generate_initial_events();
                if billed {
                    let _ = ctx.process_kiro_event(&metering(0.3));
                }

                let events = ctx.finish_at_eof();
                assert_eq!(events.len(), 1, "只发 error 事件，不伪装成正常收尾");
                assert_eq!(events[0].event, "error");
                assert_eq!(events[0].data["error"]["type"], error_type);

                let page = tracker.get_records_paged(1, 1, 10, &RecordLabels::default());
                if billed {
                    assert_eq!(page.records.len(), 1);
                    assert_eq!(page.records[0].error, Some(usage_error));
                } else {
                    assert!(page.records.is_empty(), "上游未计费不记录");
                }
                let _ = std::fs::remove_dir_all(&dir);
            }
        }
    }

    #[tokio::test]
    async fn test_eof_completed_records_usage_without_error() {
        use crate::model::usage::{RecordLabels, UsageTracker};

        let dir = std::env::temp_dir().join(format!("kiro2cc_ctx_ok_{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let tracker = std::sync::Arc::new(UsageTracker::load(&dir.join("usage.json")).unwrap());
        let mut ctx = StreamContext::new_with_thinking("test-model", 1, false).with_usage_tracking(
            Some(std::sync::Arc::clone(&tracker)),
            Some(1),
            Some(7),
            None,
            None,
        );
        let _ = ctx.generate_initial_events();
        ctx.process_assistant_response("hello");

        let events = ctx.finish_at_eof();
        assert_eq!(events.last().unwrap().event, "message_stop");
        let page = tracker.get_records_paged(1, 1, 10, &RecordLabels::default());
        assert_eq!(page.records.len(), 1, "正常完成未计费也照常记录");
        assert_eq!(page.records[0].error, None);
        let _ = std::fs::remove_dir_all(&dir);
    }

    fn delta_usage(events: &[crate::anthropic::stream::SseEvent]) -> &serde_json::Value {
        &events
            .iter()
            .find(|e| e.event == "message_delta")
            .unwrap()
            .data["usage"]
    }

    #[tokio::test]
    async fn test_stream_cache_read_from_credits_is_shared_by_client_and_log() {
        use crate::anthropic::stream::calib::scale_for_client;

        let dir = std::env::temp_dir().join(format!("kiro2cc_ctx_refine_{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("api_key_usage.json");
        let tracker = std::sync::Arc::new(crate::model::usage::UsageTracker::load(&path).unwrap());

        let key_id = 1;
        let model = "claude-haiku-5-5";
        let mut ctx = StreamContext::new_with_thinking(model, 53_776, false).with_usage_tracking(
            Some(std::sync::Arc::clone(&tracker)),
            Some(key_id),
            Some(7),
            None,
            None,
        );

        // message_start 尚无 credits：报完整输入、缓存为 0
        let start = ctx.create_message_start_event();
        let start_usage = &start["message"]["usage"];
        assert_eq!(start_usage["input_tokens"], scale_for_client(53_776, model));
        assert_eq!(start_usage["cache_read_input_tokens"], 0);
        assert_eq!(start_usage["cache_creation_input_tokens"], 0);
        let _ = ctx.generate_initial_events();

        // 346 tokens 输出（BPE 8 个 'a' 合并 1 token）与约 39% 命中的 meteringEvent
        ctx.output_full_text = "a".repeat(346 * 8);
        let _ = ctx.process_kiro_event(&crate::kiro::model::events::Event::Metering(
            crate::kiro::model::events::MeteringEvent {
                usage: 0.06475,
                unit_plural: "credits".to_string(),
                unit: String::new(),
                cache_read_input_tokens: None,
                cache_creation_input_tokens: None,
            },
        ));
        let (final_events, logs) = capture_debug_logs(|| ctx.generate_final_events());
        assert!(logs.contains("source=Credits"), "{logs}");

        let page =
            tracker.get_records_paged(key_id, 1, 10, &crate::model::usage::RecordLabels::default());
        assert_eq!(page.records.len(), 1);
        let recorded_read = page.records[0].cache_read_input_tokens.unwrap();
        assert!(
            (20_000..=22_500).contains(&recorded_read),
            "{recorded_read}"
        );
        assert_eq!(page.records[0].cache_creation_input_tokens, Some(0));

        // message_delta 与用量日志同一个值
        let usage = delta_usage(&final_events);
        assert_eq!(
            usage["cache_read_input_tokens"],
            scale_for_client(recorded_read, model)
        );
        assert_eq!(
            usage["input_tokens"],
            scale_for_client(53_776 - recorded_read, model)
        );
        assert_eq!(usage["cache_creation_input_tokens"], 0);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn test_stream_cache_read_falls_back_to_same_account_prefix() {
        use crate::cache::{PrefixCache, PrefixProbe, PrefixProfile};
        use crate::kiro::model::requests::conversation::{
            ConversationState, CurrentMessage, HistoryAssistantMessage, HistoryUserMessage,
            Message, UserInputMessage,
        };

        // 未标定计费曲线的模型：credits 无法反推，按同账号前缀估算
        let model = "claude-sonnet-4.6";
        let system = "s".repeat(9_000);
        let state = ConversationState::new("c")
            .with_history(vec![
                Message::User(HistoryUserMessage::new(system, model)),
                Message::Assistant(HistoryAssistantMessage::new("ack")),
            ])
            .with_current_message(CurrentMessage::new(UserInputMessage::new("q", model)));
        let cache = std::sync::Arc::new(PrefixCache::new());

        let finish = |account: u64| {
            let probe = PrefixProbe::new(
                std::sync::Arc::clone(&cache),
                PrefixProfile::from_state(&state).unwrap(),
            );
            let mut ctx = StreamContext::new_with_thinking(model, 10_000, false)
                .with_usage_tracking(None, None, Some(account), None, None)
                .with_prefix_probe(Some(probe));
            let _ = ctx.generate_initial_events();
            ctx.process_assistant_response("hello");
            delta_usage(&ctx.generate_final_events())["cache_read_input_tokens"]
                .as_i64()
                .unwrap()
        };

        assert_eq!(finish(1), 0, "冷启动");
        assert!(finish(1) > 0, "同账号同前缀命中");
        assert_eq!(finish(2), 0, "缓存不跨账号");
    }
}
