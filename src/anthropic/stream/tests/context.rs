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

    #[tokio::test]
    async fn test_stream_context_refines_cache_read_from_metering_credits() {
        let dir = std::env::temp_dir().join(format!("kiro2cc_ctx_refine_{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("api_key_usage.json");
        let tracker = std::sync::Arc::new(crate::model::usage::UsageTracker::load(&path).unwrap());

        let key_id = 1;
        let mut ctx = StreamContext::new_with_thinking("claude-haiku-5-5", 53_776, false)
            .with_prefix_estimated_tokens(53_650)
            .with_conversation_id(Some(format!("miss-diag-integration-{key_id}")))
            .with_usage_tracking(
                Some(std::sync::Arc::clone(&tracker)),
                Some(key_id),
                Some(7),
                None,
                None,
            );

        // message_start 阶段尚未收到 meteringEvent，保持静态前缀估算 53_650
        let (_, _, start_cache_read) =
            ctx.derive_report_cache_usage(53_776, None, &Default::default());
        assert_eq!(start_cache_read, 53_650);
        let _ = ctx.generate_initial_events();

        // 模拟 346 tokens 输出（BPE 8 个 'a' 合并 1 token，2768 = 346）与部分缓存失效的 meteringEvent
        ctx.output_full_text = "a".repeat(346 * 8);
        let metering_event = crate::kiro::model::events::Event::Metering(
            crate::kiro::model::events::MeteringEvent {
                usage: 0.06475,
                unit_plural: "credits".to_string(),
                unit: String::new(),
                cache_read_input_tokens: None,
                cache_creation_input_tokens: None,
            },
        );
        let _ = ctx.process_kiro_event(&metering_event);
        // [cache-miss] 诊断：credits 反推生效（命中率 ~39%）应触发一次
        let (final_events, diag_out) = capture_debug_logs(|| ctx.generate_final_events());
        assert_eq!(diag_out.matches("[cache-miss]").count(), 1, "{diag_out}");
        assert!(
            diag_out.contains("session=miss-diag-integration-1"),
            "{diag_out}"
        );

        // 1. 验证 UsageTracker 落库的 cache_read_input_tokens 已按 creditsUsed 反推下修
        let page =
            tracker.get_records_paged(key_id, 1, 10, &crate::model::usage::RecordLabels::default());
        assert_eq!(page.records.len(), 1);
        let recorded_read = page.records[0].cache_read_input_tokens.unwrap();
        assert!((20_000..=22_500).contains(&recorded_read));
        assert_eq!(page.records[0].credits_used, Some(0.06475));

        // 2. 末尾 message_delta 保持静态前缀口径，与 message_start 一致
        let delta_evt = final_events
            .iter()
            .find(|e| e.event == "message_delta")
            .unwrap();
        let delta_read = delta_evt.data["usage"]["cache_read_input_tokens"]
            .as_i64()
            .unwrap() as i32;
        assert_eq!(
            delta_read,
            crate::anthropic::stream::calib::scale_for_client(53_650, "claude-haiku-5-5")
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[tokio::test]
    async fn test_stream_context_accounting_precedes_credit_inference() {
        use crate::cache::breakpoint::{BreakpointCache, CacheAccounting};
        use crate::kiro::model::events::{Event, MeteringEvent};

        let cache = std::sync::Arc::new(BreakpointCache::new(1, None));
        let body = bytes::Bytes::from(
            serde_json::to_vec(&serde_json::json!({
                "model": "claude-haiku-5-5",
                "system": [{
                    "type": "text",
                    "text": "stable prefix ".repeat(100),
                    "cache_control": {"type": "ephemeral", "ttl": "1h"}
                }],
                "messages": [{"role": "user", "content": "question"}]
            }))
            .unwrap(),
        );
        for cached in [false, true] {
            let accounting = CacheAccounting::from_request(
                &cache,
                body.clone(),
                "test-key".into(),
                "claude-haiku-5-5".into(),
            )
            .await
            .unwrap();
            let expected = accounting.usage_for(53_776);
            if cached {
                assert!(expected.cache_read_input_tokens > 0);
            } else {
                assert!(expected.cache_creation_input_tokens > 0);
            }
            let mut ctx = StreamContext::new_with_thinking("claude-haiku-5-5", 53_776, false)
                .with_prefix_estimated_tokens(53_650)
                .with_cache_accounting(Some(accounting.clone()));
            assert_eq!(
                ctx.derive_report_cache_usage(53_776, Some(9.99), &Default::default()),
                (
                    expected.input_tokens,
                    expected.cache_creation_input_tokens,
                    expected.cache_read_input_tokens
                )
            );
            ctx.process_kiro_event(&Event::Metering(MeteringEvent {
                usage: 9.99,
                unit_plural: "credits".into(),
                unit: String::new(),
                cache_read_input_tokens: Some(600),
                cache_creation_input_tokens: Some(200),
            }));
            assert_eq!(
                ctx.derive_report_cache_usage(53_776, Some(9.99), &Default::default()),
                (52_976, 200, 600)
            );
            accounting.commit();
        }
    }
}
