//! 用量追踪模块测试（原 usage.rs 内联测试搬移）

#[cfg(test)]
mod tests {

    use crate::model::usage::{
        BilledOutput, RecordLabels, RecordOptions, ReplayedReasoning, UsageTracker, calculate_cost,
        get_k_ref, infer_cache_read_from_credits, refine_prefix_cache_read,
    };

    /// 每个测试使用独立目录隔离明细文件与累计基数文件（api_key_lifetime.json
    /// 固定落在明细同目录，仅隔离文件名会让所有测试共享/互踩同一累计文件）
    fn temp_usage_path(name: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "kiro2cc_usage_test_{}_{}",
            name,
            uuid::Uuid::new_v4()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        dir.join("api_key_usage.json")
    }

    /// 清理测试独立目录（明细与累计基数同目录）
    fn cleanup_usage_path(path: &std::path::Path) {
        if let Some(dir) = path.parent() {
            let _ = std::fs::remove_dir_all(dir);
        }
    }

    #[tokio::test]
    async fn test_get_total_credits_uses_real_credits_when_present() {
        let path = temp_usage_path("credits_present");
        let tracker = UsageTracker::load(&path).unwrap();
        // credits_used 显式提供时，应直接累加该值而非回退估算
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            1000,
            100,
            RecordOptions {
                credits_used: Some(3.43),
                ..Default::default()
            },
        );
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            1000,
            100,
            RecordOptions {
                credits_used: Some(1.0),
                ..Default::default()
            },
        );
        assert!((tracker.get_total_credits(1) - 4.43).abs() < 1e-9);
        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_get_total_credits_falls_back_to_estimated_cost_times_k_ref() {
        let path = temp_usage_path("credits_fallback");
        let tracker = UsageTracker::load(&path).unwrap();
        // 旧记录无 credits_used 时按 estimated_cost * k_ref 回退估算
        tracker.record(
            1,
            "claude-sonnet-4.5".to_string(),
            1_000_000,
            0,
            RecordOptions::default(),
        );
        let expected_cost = calculate_cost("claude-sonnet-4.5", 1_000_000, 0);
        let expected_credits = expected_cost * get_k_ref("claude-sonnet-4.5");
        assert!((tracker.get_total_credits(1) - expected_credits).abs() < 1e-9);
        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_get_total_credits_only_counts_matching_api_key() {
        let path = temp_usage_path("credits_scoped");
        let tracker = UsageTracker::load(&path).unwrap();
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            100,
            10,
            RecordOptions {
                credits_used: Some(5.0),
                ..Default::default()
            },
        );
        tracker.record(
            2,
            "claude-opus-4.6".to_string(),
            100,
            10,
            RecordOptions {
                credits_used: Some(99.0),
                ..Default::default()
            },
        );
        assert!((tracker.get_total_credits(1) - 5.0).abs() < 1e-9);
        assert!((tracker.get_total_credits(2) - 99.0).abs() < 1e-9);
        assert_eq!(tracker.get_total_credits(3), 0.0);
        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_get_summary_total_credits_matches_get_total_credits() {
        let path = temp_usage_path("summary_credits");
        let tracker = UsageTracker::load(&path).unwrap();
        tracker.record(
            1,
            "claude-opus-4.8".to_string(),
            500,
            50,
            RecordOptions {
                credits_used: Some(2.5),
                ..Default::default()
            },
        );
        let summary = tracker.get_summary(1);
        assert!((summary.total_credits - tracker.get_total_credits(1)).abs() < 1e-9);
        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_record_effort_persisted_and_none_fallback() {
        let path = temp_usage_path("effort_field");
        let tracker = UsageTracker::load(&path).unwrap();
        tracker.record(
            1,
            "claude-opus-4.8".to_string(),
            100,
            10,
            RecordOptions {
                effort: Some("xhigh".to_string()),
                thinking_budget_tokens: Some(12000),
                ..Default::default()
            },
        );
        tracker.record(
            1,
            "claude-sonnet-4.5".to_string(),
            100,
            10,
            RecordOptions::default(),
        );
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            100,
            10,
            RecordOptions {
                credential_id: Some(7),
                first_token_ms: Some(1234),
                duration_ms: Some(5678),
                ..Default::default()
            },
        );
        let page = tracker.get_records_paged(1, 1, 10, &RecordLabels::default());
        assert_eq!(page.records.len(), 3);
        // 有 effort 的记录映射到 UsageRecordItem
        let with_effort = page
            .records
            .iter()
            .find(|r| r.effort.as_deref() == Some("xhigh"))
            .expect("带 effort 的记录应在分页结果中");
        // thinking 预算随记录透传到 DTO
        assert_eq!(with_effort.thinking_budget_tokens, Some(12000));
        // None 兜底：未传 output_config 的旧请求 effort 保持 None
        assert!(page.records.iter().any(|r| r.effort.is_none()));
        // 带耗时的记录：record() 入参 → 分页 DTO 的 Some 值透传（按值 find 定位，
        // 不依赖 newest-first 排序在纳秒同刻并列时的稳定性）
        let timed = page
            .records
            .iter()
            .find(|r| r.first_token_ms == Some(1234))
            .expect("带耗时的记录应在分页结果中");
        assert_eq!(timed.duration_ms, Some(5678));
        // 未传耗时的旧记录兜底为 None（前两条 effort 测试记录均无耗时）
        assert!(
            page.records[1..3]
                .iter()
                .all(|r| r.first_token_ms.is_none() && r.duration_ms.is_none())
        );
        cleanup_usage_path(&path);
    }

    #[test]
    fn test_usage_record_duration_serde_compat() {
        // 新字段序列化：camelCase + None 时省略
        let record = crate::model::usage::UsageRecord {
            api_key_id: 1,
            credential_id: None,
            model: "claude-opus-4.6".to_string(),
            input_tokens: 100,
            output_tokens: 10,
            estimated_cost: 0.0,
            credits_used: None,
            cache_read_input_tokens: None,
            cache_creation_input_tokens: None,
            cache_creation_5m_input_tokens: 0,
            cache_creation_1h_input_tokens: 0,
            created_at: chrono::Utc::now(),
            client_ip: None,
            effort: None,
            thinking_budget_tokens: None,
            first_token_ms: Some(1234),
            duration_ms: Some(5678),
            thinking_token_count: None,
            is_stream: None,
            user_agent: None,
            thinking_loss: None,
            api_format: None,
            error: None,
        };
        let json = serde_json::to_value(&record).unwrap();
        assert_eq!(json["firstTokenMs"], 1234);
        assert_eq!(json["durationMs"], 5678);
        // None 时省略（与既有 Option 字段同构）
        assert!(json.get("thinkingTokenCount").is_none());
        assert!(json.get("isStream").is_none());

        // 旧数据（无新字段）反序列化兼容
        let old_json = serde_json::json!({
            "apiKeyId": 1,
            "model": "claude-opus-4.6",
            "inputTokens": 100,
            "outputTokens": 10,
            "estimatedCost": 0.0,
            "cacheCreation5mInputTokens": 0,
            "cacheCreation1hInputTokens": 0,
            "createdAt": "2026-01-01T00:00:00Z"
        });
        let parsed: crate::model::usage::UsageRecord = serde_json::from_value(old_json).unwrap();
        assert_eq!(parsed.first_token_ms, None);
        assert_eq!(parsed.duration_ms, None);
        assert_eq!(parsed.thinking_token_count, None);
        assert_eq!(parsed.is_stream, None);
    }

    #[test]
    fn test_usage_record_thinking_token_serde_compat() {
        // Some 值序列化为 camelCase thinkingTokenCount
        let record = crate::model::usage::UsageRecord {
            api_key_id: 1,
            credential_id: None,
            model: "claude-opus-4.6".to_string(),
            input_tokens: 100,
            output_tokens: 10,
            estimated_cost: 0.0,
            credits_used: None,
            cache_read_input_tokens: None,
            cache_creation_input_tokens: None,
            cache_creation_5m_input_tokens: 0,
            cache_creation_1h_input_tokens: 0,
            created_at: chrono::Utc::now(),
            client_ip: None,
            effort: None,
            thinking_budget_tokens: None,
            first_token_ms: None,
            duration_ms: None,
            thinking_token_count: Some(1034),
            is_stream: Some(true),
            user_agent: Some("claude-cli/2.1.5 (external, cli)".to_string()),
            thinking_loss: None,
            api_format: None,
            error: None,
        };
        let json = serde_json::to_value(&record).unwrap();
        assert_eq!(json["thinkingTokenCount"], 1034);
        assert_eq!(json["userAgent"], "claude-cli/2.1.5 (external, cli)");
        assert_eq!(json["isStream"], true);

        // 旧数据（无该字段）反序列化为 None
        let old_json = serde_json::json!({
            "apiKeyId": 1,
            "model": "claude-opus-4.6",
            "inputTokens": 100,
            "outputTokens": 10,
            "estimatedCost": 0.0,
            "cacheCreation5mInputTokens": 0,
            "cacheCreation1hInputTokens": 0,
            "createdAt": "2026-01-01T00:00:00Z"
        });
        let parsed: crate::model::usage::UsageRecord = serde_json::from_value(old_json).unwrap();
        assert_eq!(parsed.thinking_token_count, None);
        assert_eq!(parsed.is_stream, None);
        assert_eq!(parsed.user_agent, None);
    }

    /// user_agent 落库与分页透传：UA 值应完整传递到分页 DTO
    #[tokio::test]
    async fn test_record_user_agent_persisted_and_paged() {
        let path = temp_usage_path("user_agent_field");
        let tracker = UsageTracker::load(&path).unwrap();
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            100,
            10,
            RecordOptions {
                user_agent: Some("claude-cli/2.1.5 (external, cli)".to_string()),
                ..Default::default()
            },
        );
        let page = tracker.get_records_paged(1, 1, 10, &RecordLabels::default());
        assert_eq!(page.records.len(), 1);
        assert_eq!(
            page.records[0].user_agent.as_deref(),
            Some("claude-cli/2.1.5 (external, cli)")
        );
        // daily 路径透传：同一条记录经 get_records_paged_by_date 查询 UA 同样在
        let cst_today = {
            let cst = chrono::FixedOffset::east_opt(8 * 3600).unwrap();
            chrono::Utc::now()
                .with_timezone(&cst)
                .format("%Y-%m-%d")
                .to_string()
        };
        let daily = tracker.get_records_paged_by_date(&cst_today, 1, 10, &RecordLabels::default());
        assert_eq!(daily.records.len(), 1);
        assert_eq!(
            daily.records[0].user_agent.as_deref(),
            Some("claude-cli/2.1.5 (external, cli)")
        );
        cleanup_usage_path(&path);
    }

    /// thinking_loss：有丢失才落库并透传到分页 DTO；旧数据与无丢失都省略该字段
    #[tokio::test]
    async fn test_record_thinking_loss_persisted_and_paged() {
        use crate::model::usage::ThinkingLoss;

        assert_eq!(ThinkingLoss::default().non_empty(), None);
        let loss = ThinkingLoss {
            unsigned_turns: 2,
            signature_rejected: true,
            ..Default::default()
        };
        assert_eq!(
            serde_json::to_value(loss).unwrap(),
            serde_json::json!({"unsignedTurns": 2, "signatureRejected": true})
        );

        let path = temp_usage_path("thinking_loss_field");
        let tracker = UsageTracker::load(&path).unwrap();
        for thinking_loss in [loss.non_empty(), None] {
            tracker.record(
                1,
                "claude-opus-4.6".to_string(),
                100,
                10,
                RecordOptions {
                    thinking_loss,
                    ..Default::default()
                },
            );
        }
        let page = tracker.get_records_paged(1, 1, 10, &RecordLabels::default());
        let json = serde_json::to_value(&page.records).unwrap();
        let losses: Vec<_> = json
            .as_array()
            .unwrap()
            .iter()
            .filter_map(|r| r.get("thinkingLoss"))
            .collect();
        assert_eq!(
            losses,
            [&serde_json::json!({"unsignedTurns": 2, "signatureRejected": true})]
        );

        let old_json = serde_json::json!({
            "apiKeyId": 1,
            "model": "claude-opus-4.6",
            "inputTokens": 100,
            "outputTokens": 10,
            "estimatedCost": 0.0,
            "createdAt": "2026-01-01T00:00:00Z"
        });
        let parsed: crate::model::usage::UsageRecord = serde_json::from_value(old_json).unwrap();
        assert_eq!(parsed.thinking_loss, None);
        assert_eq!(parsed.api_format, None);
        cleanup_usage_path(&path);
    }

    /// api_format：按 kebab-case 落库并透传到分页 DTO
    #[tokio::test]
    async fn test_record_api_format_persisted_and_paged() {
        use crate::model::usage::ApiFormat;

        let path = temp_usage_path("api_format_field");
        let tracker = UsageTracker::load(&path).unwrap();
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            100,
            10,
            RecordOptions {
                api_format: Some(ApiFormat::OpenaiResponses),
                ..Default::default()
            },
        );
        let page = tracker.get_records_paged(1, 1, 10, &RecordLabels::default());
        let json = serde_json::to_value(&page.records).unwrap();
        assert_eq!(json[0]["apiFormat"], "openai-responses");
        cleanup_usage_path(&path);
    }

    /// 构造指定 UTC 时间戳的记录并直接注入（绕过 record() 的 Utc::now()，
    /// 供 date_range 系列测试控制记录的 CST 归属日）
    fn inject_record_at(tracker: &UsageTracker, created_at: chrono::DateTime<chrono::Utc>) {
        let record = crate::model::usage::UsageRecord {
            api_key_id: 1,
            credential_id: None,
            model: "claude-opus-4.6".to_string(),
            input_tokens: 100,
            output_tokens: 10,
            estimated_cost: 0.0,
            credits_used: None,
            cache_read_input_tokens: None,
            cache_creation_input_tokens: None,
            cache_creation_5m_input_tokens: 0,
            cache_creation_1h_input_tokens: 0,
            created_at,
            client_ip: None,
            effort: None,
            thinking_budget_tokens: None,
            first_token_ms: None,
            duration_ms: None,
            thinking_token_count: None,
            is_stream: None,
            user_agent: None,
            thinking_loss: None,
            api_format: None,
            error: None,
        };
        tracker.records.write().push(record);
    }

    /// 解析 RFC3339 UTC 时间戳（比手拼 NaiveDateTime+TimeZone 可读）
    fn utc_at(s: &str) -> chrono::DateTime<chrono::Utc> {
        chrono::DateTime::parse_from_rfc3339(s)
            .unwrap()
            .with_timezone(&chrono::Utc)
    }

    /// 注入归属指定账号、指定时间戳的记录（用于验证「不限日期」聚合确实跨日累加，
    /// 以及账号隔离）。固定 100 输入 / 10 输出 token，便于断言行数。
    fn inject_credential_record_at(
        tracker: &UsageTracker,
        credential_id: u64,
        created_at: chrono::DateTime<chrono::Utc>,
        estimated_cost: f64,
        credits_used: Option<f64>,
    ) {
        let record = crate::model::usage::UsageRecord {
            api_key_id: 1,
            credential_id: Some(credential_id),
            model: "claude-opus-4.6".to_string(),
            input_tokens: 100,
            output_tokens: 10,
            estimated_cost,
            credits_used,
            cache_read_input_tokens: None,
            cache_creation_input_tokens: None,
            cache_creation_5m_input_tokens: 0,
            cache_creation_1h_input_tokens: 0,
            created_at,
            client_ip: None,
            effort: None,
            thinking_budget_tokens: None,
            first_token_ms: None,
            duration_ms: None,
            thinking_token_count: None,
            is_stream: None,
            user_agent: None,
            thinking_loss: None,
            api_format: None,
            error: None,
        };
        tracker.records.write().push(record);
    }

    /// date_range：from == to 时区间版与单日版结果等价（单日版已改为区间版薄包装，
    /// 此测试锁住等价性防回归）。
    /// 等价性前提：结果未触及截断上限——单日版截断 2000 / 区间版截断 10000，
    /// 总条数落在 2001~10000 时两者不等价，不在本测试覆盖范围
    #[tokio::test]
    async fn test_date_range_from_eq_to_equivalent_to_daily() {
        let path = temp_usage_path("date_range_equiv");
        let tracker = UsageTracker::load(&path).unwrap();
        // 三天（CST）各两条
        inject_record_at(&tracker, utc_at("2026-10-01T02:00:00Z")); // CST 10-01 10:00
        inject_record_at(&tracker, utc_at("2026-10-01T10:00:00Z")); // CST 10-01 18:00
        inject_record_at(&tracker, utc_at("2026-10-02T02:00:00Z"));
        inject_record_at(&tracker, utc_at("2026-10-02T10:00:00Z"));
        inject_record_at(&tracker, utc_at("2026-10-03T02:00:00Z"));
        inject_record_at(&tracker, utc_at("2026-10-03T10:00:00Z"));

        for date in ["2026-10-01", "2026-10-02", "2026-10-03"] {
            let daily = tracker.get_records_paged_by_date(date, 1, 10, &RecordLabels::default());
            let ranged = tracker.get_records_paged_by_date_range(
                date,
                date,
                1,
                10,
                &RecordLabels::default(),
            );
            assert_eq!(ranged.total, daily.total);
            assert_eq!(ranged.total_pages, daily.total_pages);
            assert_eq!(ranged.page, daily.page);
            assert_eq!(ranged.page_size, daily.page_size);
            let ranged_times: Vec<_> = ranged.records.iter().map(|r| r.created_at).collect();
            let daily_times: Vec<_> = daily.records.iter().map(|r| r.created_at).collect();
            assert_eq!(ranged_times, daily_times);
        }
        cleanup_usage_path(&path);
    }

    /// date_range：闭区间含两端日，边界日各含记录时全部返回且按时间降序
    #[tokio::test]
    async fn test_date_range_inclusive_boundary_days() {
        let path = temp_usage_path("date_range_boundary");
        let tracker = UsageTracker::load(&path).unwrap();
        inject_record_at(&tracker, utc_at("2026-10-01T16:00:00Z")); // CST 10-02 00:00（区间首日）
        inject_record_at(&tracker, utc_at("2026-10-01T18:00:00Z")); // CST 10-02 02:00
        inject_record_at(&tracker, utc_at("2026-10-02T18:00:00Z")); // CST 10-03 02:00（中间日）
        inject_record_at(&tracker, utc_at("2026-10-03T16:00:00Z")); // CST 10-04 00:00（区间末日）
        inject_record_at(&tracker, utc_at("2026-10-05T02:00:00Z")); // CST 10-05（区间外）

        let page = tracker.get_records_paged_by_date_range(
            "2026-10-02",
            "2026-10-04",
            1,
            10,
            &RecordLabels::default(),
        );
        assert_eq!(page.total, 4);
        assert_eq!(page.total_pages, 1);
        let times: Vec<_> = page.records.iter().map(|r| r.created_at).collect();
        assert!(times.windows(2).all(|w| w[0] >= w[1])); // 降序
        assert!(times.contains(&utc_at("2026-10-01T16:00:00Z"))); // 首日边界含
        assert!(times.contains(&utc_at("2026-10-03T16:00:00Z"))); // 末日边界含
        cleanup_usage_path(&path);
    }

    /// date_range：跨 CST 午夜记录按 CST 归属日过滤（UTC 15:30/16:30 分归 CST 前后日）
    #[tokio::test]
    async fn test_date_range_cross_cst_midnight_attribution() {
        let path = temp_usage_path("date_range_midnight");
        let tracker = UsageTracker::load(&path).unwrap();
        inject_record_at(&tracker, utc_at("2026-10-01T15:30:00Z")); // CST 10-01 23:30
        inject_record_at(&tracker, utc_at("2026-10-01T16:30:00Z")); // CST 10-02 00:30

        let day1 = tracker.get_records_paged_by_date_range(
            "2026-10-01",
            "2026-10-01",
            1,
            10,
            &RecordLabels::default(),
        );
        assert_eq!(day1.total, 1);
        assert_eq!(day1.records[0].created_at, utc_at("2026-10-01T15:30:00Z"));

        let day2 = tracker.get_records_paged_by_date_range(
            "2026-10-02",
            "2026-10-02",
            1,
            10,
            &RecordLabels::default(),
        );
        assert_eq!(day2.total, 1);
        assert_eq!(day2.records[0].created_at, utc_at("2026-10-01T16:30:00Z"));

        let both = tracker.get_records_paged_by_date_range(
            "2026-10-01",
            "2026-10-02",
            1,
            10,
            &RecordLabels::default(),
        );
        assert_eq!(both.total, 2);
        assert_eq!(both.records[0].created_at, utc_at("2026-10-01T16:30:00Z")); // 降序：更晚在前
        assert_eq!(both.records[1].created_at, utc_at("2026-10-01T15:30:00Z"));
        cleanup_usage_path(&path);
    }

    /// date_range：无匹配记录返回空页（total=0、total_pages=0）
    #[tokio::test]
    async fn test_date_range_empty_result() {
        let path = temp_usage_path("date_range_empty");
        let tracker = UsageTracker::load(&path).unwrap();
        inject_record_at(&tracker, utc_at("2026-10-05T02:00:00Z"));
        let page = tracker.get_records_paged_by_date_range(
            "2026-10-01",
            "2026-10-03",
            1,
            10,
            &RecordLabels::default(),
        );
        assert!(page.records.is_empty());
        assert_eq!(page.total, 0);
        assert_eq!(page.total_pages, 0);
        assert_eq!(page.page, 1);
        cleanup_usage_path(&path);
    }

    /// date_range：page_size 越界钳制到 [1, 500]（与单日端点一致）
    #[tokio::test]
    async fn test_date_range_page_size_clamp() {
        let path = temp_usage_path("date_range_clamp");
        let tracker = UsageTracker::load(&path).unwrap();
        inject_record_at(&tracker, utc_at("2026-10-02T02:00:00Z"));
        inject_record_at(&tracker, utc_at("2026-10-02T06:00:00Z"));
        inject_record_at(&tracker, utc_at("2026-10-02T10:00:00Z"));

        let zero = tracker.get_records_paged_by_date_range(
            "2026-10-02",
            "2026-10-02",
            1,
            0,
            &RecordLabels::default(),
        );
        assert_eq!(zero.page_size, 1);
        assert_eq!(zero.records.len(), 1);
        assert_eq!(zero.total_pages, 3);

        let huge = tracker.get_records_paged_by_date_range(
            "2026-10-02",
            "2026-10-02",
            1,
            9999,
            &RecordLabels::default(),
        );
        assert_eq!(huge.page_size, 500);
        assert_eq!(huge.records.len(), 3);
        assert_eq!(huge.total_pages, 1);
        cleanup_usage_path(&path);
    }

    /// date_range：区间匹配超 10000 条时降序保留最新 10000 条再分页
    #[tokio::test]
    async fn test_date_range_truncate_over_limit() {
        let path = temp_usage_path("date_range_truncate");
        let tracker = UsageTracker::load(&path).unwrap();
        // 注入 10005 条（CST 10-02 全天，毫秒递增）：最早的 5 条应被截掉
        let base = utc_at("2026-10-02T00:00:00Z");
        for i in 0..10005 {
            inject_record_at(&tracker, base + chrono::Duration::milliseconds(i));
        }
        let first = tracker.get_records_paged_by_date_range(
            "2026-10-02",
            "2026-10-02",
            1,
            100,
            &RecordLabels::default(),
        );
        assert_eq!(first.total, 10_000);
        assert_eq!(first.total_pages, 100);
        assert_eq!(
            first.records[0].created_at,
            base + chrono::Duration::milliseconds(10004)
        ); // 首页首条 = 全局最新
        let last = tracker.get_records_paged_by_date_range(
            "2026-10-02",
            "2026-10-02",
            100,
            100,
            &RecordLabels::default(),
        );
        assert_eq!(last.records.len(), 100);
        assert_eq!(
            last.records[99].created_at,
            base + chrono::Duration::milliseconds(5)
        ); // 末页末条 = 截断后最旧（i=5，恰被截掉最早 5 条）

        let daily =
            tracker.get_records_paged_by_date("2026-10-02", 1, 100, &RecordLabels::default());
        assert_eq!(daily.total, 2000);
        let summaries = tracker.get_daily_summaries();
        assert_eq!(summaries.len(), 1);
        assert_eq!(summaries[0].total_requests, 10005);
        assert_eq!(summaries[0].total_input_tokens, 1_000_500);
        assert_eq!(summaries[0].total_output_tokens, 100_050);
        cleanup_usage_path(&path);
    }

    #[test]
    fn test_get_k_ref_opus_5() {
        assert_eq!(get_k_ref("claude-opus-5"), 2.36);
        assert_eq!(get_k_ref("claude-opus-5-thinking"), 2.36);
        assert_eq!(get_k_ref("Claude-Opus-5"), 2.36);

        // 回归：其他档位不变
        assert_eq!(get_k_ref("claude-opus-4-7"), 2.36);
        assert_eq!(get_k_ref("claude-opus-4-8"), 2.36);
        assert_eq!(get_k_ref("claude-opus-4-6"), 1.90);
        assert_eq!(get_k_ref("claude-opus-4-5"), 1.90);
        assert_eq!(get_k_ref("claude-sonnet-5"), 1.43);
        assert_eq!(get_k_ref("claude-sonnet-4.6"), 1.43);
        assert_eq!(get_k_ref("claude-haiku-4.5"), 1.43);
    }

    #[test]
    fn test_get_k_ref_haiku_5_5() {
        // haiku 5.5 沿用 haiku 默认档（暂无 credits 实测系数）
        assert_eq!(get_k_ref("claude-haiku-5.5"), 1.43);
        assert_eq!(get_k_ref("claude-haiku-5-5"), 1.43);
        assert_eq!(get_k_ref("claude-haiku-5-5-thinking"), 1.43);
    }

    #[test]
    fn test_pricing_haiku_5_5() {
        // 低档（≤100K）：1M output 单独不触发跳档（阈值只判 input）
        assert!((calculate_cost("claude-haiku-5.5", 0, 1_000_000) - 0.50).abs() < 1e-9);

        // 回归：haiku 4.5 定价不变（1 + 5 = 6.0）
        assert!((calculate_cost("claude-haiku-4.5", 1_000_000, 1_000_000) - 6.0).abs() < 1e-9);

        // 回归：连字符别名与映射层口径一致（曾按 4.5 定价高估 10 倍）
        // 取 50K 输入以留在低档，避免与跳档测试混淆
        assert!((calculate_cost("claude-haiku-5-5", 50_000, 0) - 0.005).abs() < 1e-9);
    }

    #[test]
    fn test_pricing_haiku_5_5_long_context_tier() {
        // 整档跳价：超 100K 时**整个请求**按高档，非超出部分累进
        // 200_000 input + 1_000 output = 0.5*0.2 + 2.5*0.001 = 0.1025
        assert!(
            (calculate_cost("claude-haiku-5.5", 200_000, 1_000) - 0.1025).abs() < 1e-9,
            "超阈值应整档跳价至 $0.50/$2.50"
        );

        // 边界：100_000 走低档，100_001 跳高档
        assert!(
            (calculate_cost("claude-haiku-5.5", 100_000, 0) - 0.01).abs() < 1e-9,
            "恰好 100K 仍属低档"
        );
        assert!(
            (calculate_cost("claude-haiku-5.5", 100_001, 0) - 0.0500005).abs() < 1e-9,
            "超过 100K 即整档跳价"
        );

        // 别名口径一致
        assert!(
            (calculate_cost("Claude Haiku 5.5", 200_000, 0) - 0.10).abs() < 1e-9,
            "空格别名同样跳档"
        );
        // 点号变体（回归：谓词改写曾丢失点号写法，会回退 4.5 定价高估 10 倍）
        assert!(
            (calculate_cost("claude-haiku.5.5", 200_000, 0) - 0.10).abs() < 1e-9,
            "点号别名同样跳档"
        );

        // 回归：haiku 4.5 无分档，1M 输入仍按低档单档计价
        assert!((calculate_cost("claude-haiku-4.5", 1_000_000, 0) - 1.0).abs() < 1e-9);
    }

    /// is_stream 落库与分页透传：流式/非流式两种取值均应完整传递到分页 DTO
    #[tokio::test]
    async fn test_record_is_stream_persisted_and_paged() {
        let path = temp_usage_path("is_stream_field");
        let tracker = UsageTracker::load(&path).unwrap();
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            100,
            10,
            RecordOptions {
                is_stream: Some(true),
                ..Default::default()
            },
        );
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            100,
            10,
            RecordOptions {
                is_stream: Some(false),
                ..Default::default()
            },
        );
        let page = tracker.get_records_paged(1, 1, 10, &RecordLabels::default());
        assert_eq!(page.records.len(), 2);
        // 两种取值均透传到 UsageRecordItem（按值断言，不依赖排序）
        assert!(page.records.iter().any(|r| r.is_stream == Some(true)));
        assert!(page.records.iter().any(|r| r.is_stream == Some(false)));
        cleanup_usage_path(&path);
    }

    /// 回归：明细超过 MAX_RECORDS_PER_KEY 被裁剪后，total_requests 仍持续增长
    /// （历史缺陷：直接数现存记录条数导致请求数封顶 10,000）
    #[tokio::test]
    async fn test_total_requests_not_capped_by_record_pruning() {
        let path = temp_usage_path("pruning_not_capped");
        let tracker = UsageTracker::load(&path).unwrap();
        // 记录上限 + 5 条：应裁掉最老的 5 条并累计进基数
        let total = crate::model::usage::MAX_RECORDS_PER_KEY_FOR_TEST + 5;
        for _ in 0..total {
            tracker.record(
                9,
                "claude-opus-4.6".to_string(),
                10,
                10,
                RecordOptions::default(),
            );
        }
        let summary = tracker.get_summary(9);
        assert_eq!(summary.total_requests, total as u64);
        // 现存明细恰好为上限条数
        cleanup_usage_path(&path);
    }

    /// 回归：reset 后请求数从 0 重新计数（基数同步清零）
    #[tokio::test]
    async fn test_reset_clears_lifetime_base() {
        let path = temp_usage_path("reset_clears_base");
        let tracker = UsageTracker::load(&path).unwrap();
        for _ in 0..(crate::model::usage::MAX_RECORDS_PER_KEY_FOR_TEST + 3) {
            tracker.record(
                8,
                "claude-opus-4.6".to_string(),
                10,
                10,
                RecordOptions::default(),
            );
        }
        assert!(tracker.get_summary(8).total_requests > 0);
        tracker.reset(8).unwrap();
        assert_eq!(tracker.get_summary(8).total_requests, 0);
        cleanup_usage_path(&path);
    }

    /// 回归：生命周期基数持久化后重启不丢（裁剪掉的部分仍计入请求数）
    #[tokio::test]
    async fn test_lifetime_base_persisted_across_reload() {
        let path = temp_usage_path("lifetime_persisted");
        {
            let tracker = UsageTracker::load(&path).unwrap();
            for _ in 0..(crate::model::usage::MAX_RECORDS_PER_KEY_FOR_TEST + 7) {
                tracker.record(
                    7,
                    "claude-opus-4.6".to_string(),
                    10,
                    10,
                    RecordOptions::default(),
                );
            }
            // 等待后台任务把脏数据落盘（周期 5s 太长，直接触发 shutdown 刷盘：
            // drop tracker 即关闭通道，graceful shutdown 分支会执行落盘）
        }
        // 轮询等待刷盘完成。不能仅等基数文件存在：后台先处理 interval tick 时
        // （局部 dirty 尚为 false）可能只写基数、不写明细，此时明细文件可能
        // 还未就绪；改为等待明细文件落盘且记录数达到上限，配合基数文件存在的
        // 前置条件，两者都就绪才认为 shutdown 刷盘完成
        let lifetime_path = path.parent().unwrap().join("api_key_lifetime.json");
        let mut persisted = false;
        for _ in 0..100 {
            let records_ready = std::fs::read_to_string(&path)
                .map(|c| {
                    serde_json::from_str::<Vec<crate::model::usage::UsageRecord>>(&c)
                        .map(|r| r.len() == crate::model::usage::MAX_RECORDS_PER_KEY_FOR_TEST)
                        .unwrap_or(false)
                })
                .unwrap_or(false);
            if records_ready && lifetime_path.exists() {
                persisted = true;
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        }
        assert!(
            persisted,
            "lifetime base file should be written on shutdown"
        );
        let tracker2 = UsageTracker::load(&path).unwrap();
        // 精确断言：明细（上限条）+ 基数（裁剪掉的 7 条）完整持久化，
        // 基数文件缺失或仅靠现存明细求和都会在此失败
        assert_eq!(
            tracker2.get_summary(7).total_requests,
            (crate::model::usage::MAX_RECORDS_PER_KEY_FOR_TEST + 7) as u64
        );
        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_daily_summaries_cache_fields() {
        let path = temp_usage_path("daily_cache_fields");
        let tracker = UsageTracker::load(&path).unwrap();
        assert!(tracker.get_daily_summaries().is_empty());
        // 两条记录：(input=1000, cacheRead=800) 与 (input=500, cacheRead=None)
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            1000,
            100,
            RecordOptions {
                cache_read_input_tokens: Some(800),
                cache_creation_input_tokens: Some(100),
                ..Default::default()
            },
        );
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            500,
            50,
            RecordOptions::default(),
        );

        let summaries = tracker.get_daily_summaries();
        assert_eq!(summaries.len(), 1);
        let s = &summaries[0];
        assert_eq!(s.total_requests, 2);
        assert_eq!(s.total_input_tokens, 1500);
        assert_eq!(s.total_output_tokens, 150);
        assert_eq!(s.total_cache_read_tokens, 800);
        let json = serde_json::to_value(s).unwrap();
        assert_eq!(json["totalOutputTokens"], 150);

        // 凭证维度：cacheRead None 按 0 计入
        tracker.record(
            1,
            "claude-opus-4.6".to_string(),
            200,
            20,
            RecordOptions {
                credential_id: Some(42),
                ..Default::default()
            },
        );
        let cred = tracker.get_today_summary_for_credential(42);
        assert_eq!(cred.total_requests, 1);
        assert_eq!(cred.total_input_tokens, 200);
        assert_eq!(cred.total_cache_read_tokens, 0);

        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_daily_summaries_cross_cst_midnight() {
        let path = temp_usage_path("daily_summaries_midnight");
        let tracker = UsageTracker::load(&path).unwrap();
        inject_record_at(&tracker, utc_at("2026-10-01T15:59:59Z"));
        inject_record_at(&tracker, utc_at("2026-10-01T16:00:00Z"));

        let summaries = tracker.get_daily_summaries();
        assert_eq!(summaries.len(), 2);
        assert_eq!(summaries[0].date, "2026-10-02");
        assert_eq!(summaries[1].date, "2026-10-01");
        for summary in &summaries {
            assert_eq!(summary.total_requests, 1);
            assert_eq!(summary.total_input_tokens, 100);
            assert_eq!(summary.total_output_tokens, 10);
        }
        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_daily_summaries_clamp_negative_output_tokens() {
        let path = temp_usage_path("daily_summaries_negative_output");
        let tracker = UsageTracker::load(&path).unwrap();
        for output in [-10, 10] {
            tracker.record(
                1,
                "claude-opus-4.6".to_string(),
                100,
                output,
                RecordOptions::default(),
            );
        }

        let summaries = tracker.get_daily_summaries();
        assert_eq!(summaries.len(), 1);
        assert_eq!(summaries[0].total_output_tokens, 10);
        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_summary_for_credential_covers_all_dates_and_isolates_accounts() {
        let path = temp_usage_path("cred_summary_all_dates");
        let tracker = UsageTracker::load(&path).unwrap();
        // 跨两个 CST 自然日：UTC 10-01 15:30 → CST 10-01 23:30；UTC 10-01 16:30 → CST 10-02 00:30
        inject_credential_record_at(&tracker, 7, utc_at("2026-10-01T15:30:00Z"), 1.0, Some(1.5));
        inject_credential_record_at(&tracker, 7, utc_at("2026-10-01T16:30:00Z"), 2.0, None);
        // 其他账号：过滤失效时 cost / credits_saved / tokens 都会被污染
        inject_credential_record_at(
            &tracker,
            8,
            utc_at("2026-10-01T16:30:00Z"),
            100.0,
            Some(0.0),
        );

        let summary = tracker.get_summary_for_credential(7);

        assert_eq!(summary.credential_id, 7);
        // 跨日累加：若实现误按「今日」过滤，这里只会有 0 或 1 条
        assert_eq!(summary.total_requests, 2);
        assert_eq!(summary.total_input_tokens, 200);
        assert_eq!(summary.total_output_tokens, 20);
        assert!((summary.total_cost - 3.0).abs() < 1e-9);
        // 第二笔无 credits_used，按 estimated_cost * k_ref 回退计入总 credits
        let fallback = 2.0 * get_k_ref("claude-opus-4.6");
        assert!((summary.total_credits - (1.5 + fallback)).abs() < 1e-9);
        // 节省额只来自账号 7 的第一笔，账号 8 的记录被排除
        let expected_saved = get_k_ref("claude-opus-4.6") - 1.5;
        assert!((summary.total_credits_saved - expected_saved).abs() < 1e-9);
        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_summary_for_credential_zero_when_no_records() {
        let path = temp_usage_path("cred_summary_empty");
        let tracker = UsageTracker::load(&path).unwrap();

        let summary = tracker.get_summary_for_credential(42);

        assert_eq!(summary.credential_id, 42);
        assert_eq!(summary.total_requests, 0);
        assert_eq!(summary.total_input_tokens, 0);
        assert_eq!(summary.total_output_tokens, 0);
        assert_eq!(summary.total_cache_read_tokens, 0);
        assert_eq!(summary.total_cost, 0.0);
        assert_eq!(summary.total_credits, 0.0);
        assert_eq!(summary.total_credits_saved, 0.0);
        cleanup_usage_path(&path);
    }

    /// range：零记录返回零值汇总（from/to 仍为合法区间，不报错）
    #[tokio::test]
    async fn test_range_summary_for_credential_zero_when_no_records() {
        let path = temp_usage_path("range_summary_empty");
        let tracker = UsageTracker::load(&path).unwrap();

        let s = tracker.get_range_summary_for_credential(42, 7);

        assert_eq!(s.credential_id, 42);
        assert_eq!(s.total_requests, 0);
        assert_eq!(s.total_input_tokens, 0);
        assert_eq!(s.total_output_tokens, 0);
        assert_eq!(s.total_cache_read_tokens, 0);
        assert_eq!(s.total_cost, 0.0);
        assert_eq!(s.total_credits, 0.0);
        assert_eq!(s.total_credits_saved, 0.0);
        // 锁定窗口两端语义：to_date = 今日（CST），from_date = 今日 - (days-1)
        let cst = chrono::FixedOffset::east_opt(8 * 3600).unwrap();
        let today = chrono::Utc::now().with_timezone(&cst).date_naive();
        assert_eq!(s.to_date, today.to_string());
        assert_eq!(s.from_date, (today - chrono::Duration::days(6)).to_string());
        // JSON 契约：camelCase 键名必须与前端 CredentialRangeSummary 类型一致，
        // 防止 rename_all 被移除或字段改名后前端静默拿到 undefined
        let json = serde_json::to_value(&s).unwrap();
        assert_eq!(json["credentialId"], 42);
        assert_eq!(json["fromDate"], s.from_date.as_str());
        assert_eq!(json["toDate"], today.to_string().as_str());
        assert_eq!(json["totalRequests"], 0);
        assert_eq!(json["totalInputTokens"], 0);
        assert_eq!(json["totalOutputTokens"], 0);
        assert_eq!(json["totalCost"], 0.0);
        assert_eq!(json["totalCredits"], 0.0);
        assert_eq!(json["totalCreditsSaved"], 0.0);
        assert_eq!(json["totalCacheReadTokens"], 0);
        cleanup_usage_path(&path);
    }

    /// range：区间窗口归属——「今日」记录计入，「昨日之前（超出 days-1 窗口）」不计入；
    /// 账号隔离：其他账号的区间内记录不影响本账号汇总
    #[tokio::test]
    async fn test_range_summary_for_credential_window_and_isolation() {
        let path = temp_usage_path("range_summary_window");
        let tracker = UsageTracker::load(&path).unwrap();

        // 今日（CST）记录：3 次请求
        let cst = chrono::FixedOffset::east_opt(8 * 3600).unwrap();
        let today = chrono::Utc::now().with_timezone(&cst).date_naive();
        let to_utc = |d: chrono::NaiveDate| {
            d.and_hms_opt(12, 0, 0)
                .unwrap()
                .and_local_timezone(cst)
                .unwrap()
                .with_timezone(&chrono::Utc)
        };
        let today_noon_utc = to_utc(today);

        // 窗口内但非今日：days=7 时应为 from 日当天（今日-6）
        let in_window_utc = to_utc(today - chrono::Duration::days(6));

        // 窗口外：今日-7（days=7 的 from 前一天）
        let out_window_utc = to_utc(today - chrono::Duration::days(7));

        inject_credential_record_at(&tracker, 7, today_noon_utc, 0.5, None);
        inject_credential_record_at(&tracker, 7, in_window_utc, 0.5, None);
        inject_credential_record_at(&tracker, 7, out_window_utc, 9.0, None);
        // 其他账号（区间内）：不应计入 cred 7
        inject_credential_record_at(&tracker, 8, today_noon_utc, 9.0, None);

        let s7 = tracker.get_range_summary_for_credential(7, 7);
        assert_eq!(s7.total_requests, 2); // 今日 + 窗口内非今日
        assert_eq!(s7.total_cost, 1.0); // 0.5 + 0.5，窗口外 9.0 不计入
        assert_eq!(
            s7.from_date,
            (today - chrono::Duration::days(6)).to_string()
        );

        // days=1 退化为「仅今日」：与 today 语义等价
        let s1 = tracker.get_range_summary_for_credential(7, 1);
        assert_eq!(s1.total_requests, 1);
        assert_eq!(s1.from_date, today.to_string());

        // days=30：窗口覆盖同批记录（仍是 2 条，out_window 在今日-7 落在 30 天窗口内）
        let s30 = tracker.get_range_summary_for_credential(7, 30);
        assert_eq!(s30.total_requests, 3);

        // 账号隔离
        let s8 = tracker.get_range_summary_for_credential(8, 7);
        assert_eq!(s8.total_requests, 1);

        cleanup_usage_path(&path);
    }

    /// range：days=0 兜底为 1（不 panic，等价今日）
    #[tokio::test]
    async fn test_range_summary_for_credential_days_zero_clamps_to_one() {
        let path = temp_usage_path("range_summary_zero_days");
        let tracker = UsageTracker::load(&path).unwrap();
        let cst = chrono::FixedOffset::east_opt(8 * 3600).unwrap();
        let today = chrono::Utc::now().with_timezone(&cst).date_naive();
        let noon_utc = today
            .and_hms_opt(12, 0, 0)
            .unwrap()
            .and_local_timezone(cst)
            .unwrap()
            .with_timezone(&chrono::Utc);
        inject_credential_record_at(&tracker, 9, noon_utc, 0.1, None);

        let s = tracker.get_range_summary_for_credential(9, 0);
        assert_eq!(s.total_requests, 1);
        assert_eq!(s.from_date, today.to_string());

        cleanup_usage_path(&path);
    }

    /// range：credits 聚合口径——credits_used 优先、缺失按 estimated_cost * k_ref 回退；
    /// credits_saved 仅在有真实 credits_used 时累计
    #[tokio::test]
    async fn test_range_summary_for_credential_credits_accounting() {
        let path = temp_usage_path("range_summary_credits");
        let tracker = UsageTracker::load(&path).unwrap();
        let cst = chrono::FixedOffset::east_opt(8 * 3600).unwrap();
        let today = chrono::Utc::now().with_timezone(&cst).date_naive();
        let noon_utc = today
            .and_hms_opt(12, 0, 0)
            .unwrap()
            .and_local_timezone(cst)
            .unwrap()
            .with_timezone(&chrono::Utc);

        // credits_used=Some(2.0)：credits += 2.0；saved += cost*k - 2.0
        inject_credential_record_at(&tracker, 5, noon_utc, 1.0, Some(2.0));
        // credits_used=None：credits += cost * k_ref
        inject_credential_record_at(&tracker, 5, noon_utc, 3.0, None);

        let s = tracker.get_range_summary_for_credential(5, 7);
        let k = get_k_ref("claude-opus-4.6");
        assert!((s.total_credits - (2.0 + 3.0 * k)).abs() < 1e-9);
        assert!((s.total_credits_saved - (1.0 * k - 2.0)).abs() < 1e-9);

        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_summary_for_credential_clamps_negative_tokens() {
        let path = temp_usage_path("cred_summary_neg");
        let tracker = UsageTracker::load(&path).unwrap();
        tracker.record(
            1,
            "claude-sonnet-4.6".to_string(),
            -100,
            -50,
            RecordOptions {
                credential_id: Some(7),
                ..Default::default()
            },
        );

        let summary = tracker.get_summary_for_credential(7);

        assert_eq!(summary.total_input_tokens, 0);
        assert_eq!(summary.total_output_tokens, 0);
        assert_eq!(summary.total_requests, 1);
        cleanup_usage_path(&path);
    }

    #[tokio::test]
    async fn test_summary_for_credential_accumulates_credits_saved_only_with_real_credits() {
        let path = temp_usage_path("cred_summary_saved");
        let tracker = UsageTracker::load(&path).unwrap();
        inject_credential_record_at(&tracker, 7, utc_at("2026-10-01T02:00:00Z"), 1.0, Some(0.5));
        inject_credential_record_at(&tracker, 7, utc_at("2026-10-02T02:00:00Z"), 2.0, None);

        let summary = tracker.get_summary_for_credential(7);

        // 只有带 credits_used 的记录贡献节省额；回退估算的记录不参与
        assert!((summary.total_credits_saved - (get_k_ref("claude-opus-4.6") - 0.5)).abs() < 1e-9);
        cleanup_usage_path(&path);
    }

    /// 无原生推理的计费输出
    fn plain(tokens: i32) -> BilledOutput {
        BilledOutput {
            tokens,
            ..Default::default()
        }
    }

    /// 单个原生推理块：`tokens` 含 `summary` 摘要 tokens
    fn reasoning(tokens: i32, summary: i32, signature_chars: usize) -> BilledOutput {
        BilledOutput {
            tokens,
            native_reasoning_tokens: summary,
            signature_chars,
            signature_blocks: 1,
        }
    }

    const NO_REPLAY: ReplayedReasoning = ReplayedReasoning {
        signature_chars: 0,
        blocks: 0,
    };

    fn hit_ratio(
        model: &str,
        input: i32,
        replayed: &ReplayedReasoning,
        output: &BilledOutput,
        credits: f64,
    ) -> f64 {
        infer_cache_read_from_credits(model, input, replayed, output, credits).unwrap() as f64
            / input as f64
    }

    #[test]
    fn test_pricing_infer_cache_read_measured_cold_and_repeat() {
        // pinned 单账号实测（2026-10-29，Rust 源码 system）：冷请求反推≈0，紧接的重复请求反推≈全命中
        for (model, input, credits, expected) in [
            ("claude-opus-5.5", 16_395, 0.33098, 0.0),
            ("claude-opus-5.5", 16_395, 0.09022, 1.0),
            ("claude-opus-5.5", 63_593, 1.21847, 0.0),
            ("claude-opus-5.5", 63_593, 0.33008, 1.0),
            ("claude-sonnet-5.5", 16_393, 0.21511, 0.0),
            ("claude-sonnet-5.5", 16_393, 0.05863, 1.0),
            ("claude-sonnet-5.5", 63_592, 0.79199, 0.0),
            ("claude-sonnet-5.5", 63_592, 0.21455, 1.0),
            ("claude-haiku-5.5", 16_394, 0.02911, 0.0),
            ("claude-haiku-5.5", 16_394, 0.00313, 1.0),
            ("claude-haiku-5.5", 31_755, 0.05631, 0.0),
            ("claude-haiku-5.5", 31_755, 0.00585, 1.0),
            // 长上下文档（整单 5 倍跳价，高于跳档区间只取长档）
            ("claude-haiku-5.5", 84_101, 0.74489, 0.0),
            ("claude-haiku-5.5", 84_101, 0.07559, 1.0),
            ("claude-haiku-5.5", 97_446, 0.86262, 0.0),
            ("claude-haiku-5.5", 97_446, 0.08736, 1.0),
        ] {
            let ratio = hit_ratio(model, input, &NO_REPLAY, &plain(1), credits);
            assert!(
                (ratio - expected).abs() < 0.03,
                "{model} input={input}: hit={ratio:.3}, 期望 {expected}"
            );
        }
    }

    #[test]
    fn test_pricing_infer_cache_read_haiku_tier_range() {
        // 跳档区间（本地 60K–80K）内两档都试，只有一档能解释才采用（2026-10-29 实测）
        // 70834 冷请求已跳长档：常态档解释不了（命中远低于 0），长档 ≈0 命中
        let long_cold = infer_cache_read_from_credits(
            "claude-haiku-5-5",
            70_834,
            &NO_REPLAY,
            &plain(1),
            0.62752,
        );
        assert!(long_cold.is_some_and(|read| read < 700), "{long_cold:?}");
        // 63590 冷请求仍在常态档，但长档 89% 命中也能解释；70834 长档全命中与常态档 55% 命中同价
        for (input, credits) in [(63_590, 0.11258), (70_834, 0.06385)] {
            assert_eq!(
                infer_cache_read_from_credits(
                    "claude-haiku-5-5",
                    input,
                    &NO_REPLAY,
                    &plain(1),
                    credits
                ),
                None,
                "input={input}"
            );
        }
    }

    #[test]
    fn test_pricing_infer_cache_read_hidden_thinking_from_signature() {
        // 2026-10-29 实测两轮会话（15K 随机词 system，第 2 轮回传第 1 轮推理）：
        // 首轮冷、续轮全命中；摘要远少于隐藏推理，须按签名长度补足输出
        for (model, input, replayed, output, credits, expected) in [
            (
                "claude-opus-5-5",
                14_968,
                NO_REPLAY,
                reasoning(61, 60, 792),
                0.33091,
                0.0,
            ),
            (
                "claude-opus-5-5",
                15_008,
                ReplayedReasoning {
                    signature_chars: 792,
                    blocks: 1,
                },
                reasoning(49, 48, 932),
                0.10692,
                1.0,
            ),
            (
                "claude-haiku-5-5",
                14_997,
                NO_REPLAY,
                reasoning(66, 65, 1_096),
                0.02874,
                0.0,
            ),
            (
                "claude-haiku-5-5",
                15_037,
                ReplayedReasoning {
                    signature_chars: 1_096,
                    blocks: 1,
                },
                reasoning(67, 66, 1_196),
                0.00421,
                1.0,
            ),
        ] {
            let ratio = hit_ratio(model, input, &replayed, &output, credits);
            assert!(
                (ratio - expected).abs() < 0.1,
                "{model} input={input}: hit={ratio:.3}, 期望 {expected}"
            );
        }

        // 有推理摘要却无签名：隐藏输出不可估算，跳过反推
        let unsigned = BilledOutput {
            signature_blocks: 0,
            signature_chars: 0,
            ..reasoning(49, 48, 0)
        };
        assert_eq!(
            infer_cache_read_from_credits(
                "claude-opus-5-5",
                15_008,
                &NO_REPLAY,
                &unsigned,
                0.10692
            ),
            None
        );
        assert_eq!(
            refine_prefix_cache_read(
                "claude-opus-5-5",
                14_000,
                15_008,
                &NO_REPLAY,
                &unsigned,
                Some(0.10692)
            ),
            14_000
        );
    }

    #[test]
    fn test_pricing_infer_cache_read_replayed_reasoning_as_input() {
        // sonnet 120K 输入、历史回传 20 块各 3000 字符签名，按曲线构造全命中 credits：
        // 回传推理折合约 2 万输入 token，忽略它会把全命中误判为约 94% 命中
        let replayed = ReplayedReasoning {
            signature_chars: 60_000,
            blocks: 20,
        };
        let replay_input = 1.48 * (0.2672 * 60_000.0 - 110.1 * 20.0);
        let input_mtok = (120_000.0 + replay_input) / 1_000_000.0;
        let credits = 0.00454 + 3.303 * input_mtok + 208.0 * 200.0 / 1_000_000.0;
        assert_eq!(
            refine_prefix_cache_read(
                "claude-sonnet-5-5",
                119_000,
                120_000,
                &replayed,
                &plain(200),
                Some(credits)
            ),
            119_000
        );
        let ignored = hit_ratio(
            "claude-sonnet-5-5",
            120_000,
            &NO_REPLAY,
            &plain(200),
            credits,
        );
        assert!((0.92..0.96).contains(&ignored), "hit={ignored:.3}");

        // opus 回传推理不计费：同样的回传对反推无影响
        let opus_credits = 0.00696 + 5.081 * 0.05 + 350.0 * 200.0 / 1_000_000.0;
        assert_eq!(
            infer_cache_read_from_credits(
                "claude-opus-5-5",
                50_000,
                &replayed,
                &plain(200),
                opus_credits
            ),
            infer_cache_read_from_credits(
                "claude-opus-5-5",
                50_000,
                &NO_REPLAY,
                &plain(200),
                opus_credits
            ),
        );
    }

    #[test]
    fn test_pricing_refine_prefix_cache_read_partial_and_capped() {
        // 按计费曲线构造：haiku 53776 输入中 21000 命中、346 输出 → credits 0.064747
        let refined = refine_prefix_cache_read(
            "claude-haiku-5-5",
            53_650,
            53_776,
            &NO_REPLAY,
            &plain(346),
            Some(0.064747),
        );
        assert!((20_900..=21_100).contains(&refined), "实际: {refined}");

        // 全命中反推值受静态前缀上界截断
        assert_eq!(
            refine_prefix_cache_read(
                "claude-haiku-5-5",
                52_000,
                53_776,
                &NO_REPLAY,
                &plain(346),
                Some(0.0127)
            ),
            52_000
        );

        // 全未命中（冷价）→ 下修为 0
        assert_eq!(
            refine_prefix_cache_read(
                "claude-haiku-5-5",
                53_650,
                53_776,
                &NO_REPLAY,
                &plain(0),
                Some(0.0953)
            ),
            0
        );
    }

    #[test]
    fn test_pricing_infer_cache_read_cheaper_than_cached_skipped() {
        // credits 远低于全命中价：曲线无法解释，返回 None 而非截成满额
        assert_eq!(
            infer_cache_read_from_credits(
                "claude-opus-5-5",
                50_000,
                &NO_REPLAY,
                &plain(500),
                0.001
            ),
            None
        );
        assert_eq!(
            refine_prefix_cache_read(
                "claude-opus-5-5",
                45_000,
                50_000,
                &NO_REPLAY,
                &plain(500),
                Some(0.001)
            ),
            45_000
        );
    }

    #[test]
    fn test_pricing_infer_cache_read_above_cold_is_miss() {
        // 2026-10-30 线上记录：opus-5.5 输入 110903、输出 436、2.5895 credits，
        // 命中比例约 −0.21（比冷价还贵）。此前退回静态前缀 110286（99%），应为 0
        let credits = 2.589_470_850_082_918_7;
        assert_eq!(
            infer_cache_read_from_credits(
                "claude-opus-5-5",
                110_903,
                &NO_REPLAY,
                &plain(436),
                credits
            ),
            Some(0)
        );
        assert_eq!(
            refine_prefix_cache_read(
                "claude-opus-5-5",
                110_286,
                110_903,
                &NO_REPLAY,
                &plain(436),
                Some(credits)
            ),
            0
        );

        // 同会话下一条：111622 输入、215 输出、0.779 credits，按曲线约 92% 命中，行为不变
        let next = infer_cache_read_from_credits(
            "claude-opus-5-5",
            111_622,
            &NO_REPLAY,
            &plain(215),
            0.778_959_499_170_812_5,
        )
        .unwrap();
        assert!((101_000..=103_500).contains(&next), "实际: {next}");
    }

    #[test]
    fn test_pricing_infer_cache_read_haiku_tier_above_cold_needs_both_tiers() {
        // haiku 跳档区间 70K：长上下文档 50% 命中的价格比常态档冷价还贵，
        // 但长上下文档能解释，按它反推，不因常态档判未命中
        let input = 70_000;
        let long_cold = 0.00179 + 8.835 * 0.07;
        let long_cached = 0.00128 + 0.883 * 0.07;
        let inferred = infer_cache_read_from_credits(
            "claude-haiku-5-5",
            input,
            &NO_REPLAY,
            &plain(0),
            (long_cold + long_cached) / 2.0,
        )
        .unwrap();
        assert!((34_500..=35_500).contains(&inferred), "实际: {inferred}");
        // 两档冷价都远低于该值：确定未命中
        assert_eq!(
            infer_cache_read_from_credits("claude-haiku-5-5", input, &NO_REPLAY, &plain(0), 5.0),
            Some(0)
        );
    }

    #[test]
    fn test_pricing_infer_cache_read_boundaries() {
        // 首轮 / history[0] 重置门控（prefix_estimated == 0）→ 无论 credits 多低均锁定为 0
        assert_eq!(
            refine_prefix_cache_read(
                "claude-haiku-5-5",
                0,
                50_000,
                &NO_REPLAY,
                &plain(100),
                Some(0.001)
            ),
            0
        );
        // 短输入（< 4096）跳过反推，保留静态前缀估算
        assert_eq!(
            refine_prefix_cache_read(
                "claude-haiku-5-5",
                4_000,
                4_095,
                &NO_REPLAY,
                &plain(0),
                Some(0.0074)
            ),
            4_000
        );
        assert_eq!(
            refine_prefix_cache_read(
                "claude-haiku-5-5",
                4_000,
                4_096,
                &NO_REPLAY,
                &plain(0),
                Some(0.0074)
            ),
            0
        );
        // 未标定模型（GPT / 5.5 以外的 Claude）跳过反推，保留静态前缀估算
        for model in [
            "gpt-5.6-luna",
            "claude-opus-4-6",
            "claude-sonnet-5",
            "claude-fable-5",
        ] {
            assert_eq!(
                infer_cache_read_from_credits(model, 50_000, &NO_REPLAY, &plain(100), 0.3),
                None,
                "{model}"
            );
            assert_eq!(
                refine_prefix_cache_read(model, 40_000, 50_000, &NO_REPLAY, &plain(100), Some(0.3)),
                40_000
            );
        }
        // 无 meteringEvent 时保留静态前缀估算
        assert_eq!(
            refine_prefix_cache_read(
                "claude-haiku-5-5",
                40_000,
                50_000,
                &NO_REPLAY,
                &plain(100),
                None
            ),
            40_000
        );
    }

    fn credits_opts(credits_used: f64, credential_id: u64) -> RecordOptions {
        RecordOptions {
            credits_used: Some(credits_used),
            credential_id: Some(credential_id),
            ..Default::default()
        }
    }

    /// 回归：key 自身明细超上限被裁剪后，已用 credits / cost 不倒退
    #[tokio::test]
    async fn test_spent_totals_not_reduced_by_key_pruning() {
        let path = temp_usage_path("spent_key_pruning");
        let tracker = UsageTracker::load(&path).unwrap();
        let cap = crate::model::usage::MAX_RECORDS_PER_KEY_FOR_TEST;
        for _ in 0..cap {
            tracker.record(1, "claude-opus-4.6".into(), 10, 10, credits_opts(1.0, 100));
        }
        let credits_before = tracker.get_total_credits(1);
        let cost_before = tracker.get_total_cost(1);

        tracker.record(1, "claude-opus-4.6".into(), 10, 10, credits_opts(0.1, 100));

        let cost_per_record = cost_before / cap as f64;
        assert!((tracker.get_total_credits(1) - (credits_before + 0.1)).abs() < 1e-6);
        assert!((tracker.get_total_cost(1) - (cost_before + cost_per_record)).abs() < 1e-9);
        let summary = tracker.get_summary(1);
        assert!((summary.total_credits - tracker.get_total_credits(1)).abs() < 1e-9);
        assert!((summary.total_cost - tracker.get_total_cost(1)).abs() < 1e-9);
        cleanup_usage_path(&path);
    }

    /// 回归：共享账号明细超上限时，其他 key 的记录被挤掉，其已用 credits 不归零
    #[tokio::test]
    async fn test_spent_totals_not_reduced_by_credential_pruning() {
        let path = temp_usage_path("spent_credential_pruning");
        let tracker = UsageTracker::load(&path).unwrap();
        let cap = crate::model::usage::MAX_RECORDS_PER_KEY_FOR_TEST;
        tracker.record(2, "claude-opus-4.6".into(), 10, 10, credits_opts(5.0, 200));
        let cost_before = tracker.get_total_cost(2);
        for _ in 0..cap {
            tracker.record(3, "claude-opus-4.6".into(), 10, 10, credits_opts(1.0, 200));
        }

        assert!((tracker.get_total_credits(2) - 5.0).abs() < 1e-9);
        assert!((tracker.get_total_cost(2) - cost_before).abs() < 1e-9);
        assert_eq!(tracker.get_summary(2).total_requests, 1);
        cleanup_usage_path(&path);
    }

    /// 旧版累计基数文件（无 prunedCost / prunedCredits 字段）可加载，缺失值按 0 计
    #[tokio::test]
    async fn test_legacy_lifetime_file_without_spent_fields_loads() {
        let path = temp_usage_path("legacy_lifetime");
        let lifetime = path.parent().unwrap().join("api_key_lifetime.json");
        std::fs::write(
            &lifetime,
            r#"{"4":{"prunedRequests":3,"prunedInputTokens":30,"prunedOutputTokens":30}}"#,
        )
        .unwrap();
        let tracker = UsageTracker::load(&path).unwrap();
        assert_eq!(tracker.get_summary(4).total_requests, 3);
        assert_eq!(tracker.get_total_credits(4), 0.0);
        assert_eq!(tracker.get_total_cost(4), 0.0);
        cleanup_usage_path(&path);
    }

    /// Key 汇总与分模型汇总携带缓存命中 tokens（None 按 0 计），供面板计算命中率
    #[tokio::test]
    async fn test_summary_includes_cache_read_tokens() {
        let path = temp_usage_path("summary_cache_read");
        let tracker = UsageTracker::load(&path).unwrap();
        let with_read = |read: Option<i32>| RecordOptions {
            cache_read_input_tokens: read,
            ..Default::default()
        };
        tracker.record(5, "claude-opus-4.6".into(), 1000, 10, with_read(Some(800)));
        tracker.record(5, "claude-opus-4.6".into(), 500, 10, with_read(None));
        tracker.record(5, "claude-haiku-4.5".into(), 400, 10, with_read(Some(100)));

        let summary = tracker.get_summary(5);
        assert_eq!(summary.total_input_tokens, 1900);
        assert_eq!(summary.total_cache_read_tokens, 900);
        let opus = summary
            .by_model
            .iter()
            .find(|m| m.model == "claude-opus-4.6")
            .unwrap();
        assert_eq!(opus.requests, 2);
        assert_eq!(opus.input_tokens, 1500);
        assert_eq!(opus.cache_read_tokens, 800);
        let json = serde_json::to_value(&summary).unwrap();
        assert_eq!(json["totalCacheReadTokens"], 900);
        cleanup_usage_path(&path);
    }
}
