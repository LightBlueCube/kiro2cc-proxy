// Copyright (c) 2026 Harllan He. Licensed under MIT.
// token_manager 测试（自 tests.rs 拆出，纯代码搬移）
#[cfg(test)]
pub(crate) mod tests {

    use super::super::super::entry::{DisabledReason, HealthStatus};

    use super::super::super::types::{
        MAX_FAILURES_PER_CREDENTIAL, MultiTokenManager, QUOTA_EXHAUSTED_ALL_MARKER,
    };

    use crate::kiro::model::credentials::KiroCredentials;

    use crate::kiro::token_manager::tests::ext_idp::tests::spawn_single_response_server;
    use crate::kiro::token_manager::tests::sticky::tests::make_valid_cred;
    use crate::model::config::Config;
    use chrono::{Duration, Utc};
    use std::sync::Arc;

    pub(crate) fn test_error() -> crate::kiro::token_manager::AccountError {
        crate::kiro::token_manager::AccountError::new(Some(403), "test failure")
    }

    #[test]
    fn test_multi_token_manager_new() {
        let config = Config::default();
        let mut cred1 = KiroCredentials::default();
        cred1.priority = 0;
        let mut cred2 = KiroCredentials::default();
        cred2.priority = 1;

        let manager =
            MultiTokenManager::new(config, vec![cred1, cred2], None, None, false).unwrap();
        assert_eq!(manager.total_count(), 2);
        assert_eq!(manager.available_count(), 2);
    }

    #[test]
    fn test_multi_token_manager_empty_credentials() {
        let config = Config::default();
        let result = MultiTokenManager::new(config, vec![], None, None, false);
        // 支持 0 个账号启动（可通过管理面板添加）
        assert!(result.is_ok());
        let manager = result.unwrap();
        assert_eq!(manager.total_count(), 0);
        assert_eq!(manager.available_count(), 0);
    }

    #[test]
    fn test_multi_token_manager_duplicate_ids() {
        let config = Config::default();
        let mut cred1 = KiroCredentials::default();
        cred1.id = Some(1);
        let mut cred2 = KiroCredentials::default();
        cred2.id = Some(1); // 重复 ID

        let result = MultiTokenManager::new(config, vec![cred1, cred2], None, None, false);
        assert!(result.is_err());
        let err_msg = result.err().unwrap().to_string();
        assert!(
            err_msg.contains("重复的账号 ID"),
            "错误消息应包含 '重复的账号 ID'，实际: {}",
            err_msg
        );
    }

    #[test]
    fn test_multi_token_manager_report_failure() {
        let config = Config::default();
        let cred1 = KiroCredentials::default();
        let cred2 = KiroCredentials::default();

        let manager =
            MultiTokenManager::new(config, vec![cred1, cred2], None, None, false).unwrap();

        // 账号会自动分配 ID（从 1 开始）
        // 前两次失败不会禁用（使用 ID 1）
        assert!(manager.report_failure(1, test_error()));
        assert!(manager.report_failure(1, test_error()));
        assert_eq!(manager.available_count(), 2);

        // 第三次失败会禁用第一个账号
        assert!(manager.report_failure(1, test_error()));
        assert_eq!(manager.available_count(), 1);

        // 继续失败第二个账号（使用 ID 2）
        assert!(manager.report_failure(2, test_error()));
        assert!(manager.report_failure(2, test_error()));
        assert!(!manager.report_failure(2, test_error())); // 所有账号都禁用了
        assert_eq!(manager.available_count(), 0);
    }

    #[test]
    fn test_header_success_does_not_reset_failures() {
        let config = Config::default();
        let cred = KiroCredentials::default();

        let manager = MultiTokenManager::new(config, vec![cred], None, None, false).unwrap();

        manager.report_failure(1, test_error());
        manager.report_failure(1, test_error());

        // 只拿到 2xx 响应头不清零：第 3 次失败仍禁用
        manager.report_success(1);
        manager.report_failure(1, test_error());
        assert_eq!(manager.available_count(), 0);
    }

    #[test]
    fn test_completed_response_clears_failure_count_and_last_error() {
        let config = Config::default();
        let cred = KiroCredentials::default();
        let manager = MultiTokenManager::new(config, vec![cred], None, None, false).unwrap();

        manager.report_failure(1, test_error());
        manager.report_failure(1, test_error());
        let snapshot = manager.snapshot();
        let last_error = snapshot.entries[0].last_error.as_ref().unwrap();
        assert_eq!(last_error.status_code, Some(403));
        assert_eq!(last_error.message, "test failure");

        manager.report_response_completed(1);
        let snapshot = manager.snapshot();
        assert_eq!(snapshot.entries[0].failure_count, 0);
        assert!(snapshot.entries[0].last_error.is_none());

        // 清零后再失败两次不会禁用
        manager.report_failure(1, test_error());
        manager.report_failure(1, test_error());
        assert_eq!(manager.available_count(), 1);
    }

    #[test]
    fn test_multi_token_manager_switch_to_next() {
        let config = Config::default();
        let mut cred1 = KiroCredentials::default();
        cred1.refresh_token = Some("token1".to_string());
        let mut cred2 = KiroCredentials::default();
        cred2.refresh_token = Some("token2".to_string());

        let manager =
            MultiTokenManager::new(config, vec![cred1, cred2], None, None, false).unwrap();

        // 初始是第一个账号
        assert_eq!(
            manager.credentials().refresh_token,
            Some("token1".to_string())
        );

        // 切换到下一个
        assert!(manager.switch_to_next());
        assert_eq!(
            manager.credentials().refresh_token,
            Some("token2".to_string())
        );
    }

    #[test]
    fn test_set_load_balancing_mode_persists_to_config_file() {
        let config_path =
            std::env::temp_dir().join(format!("kiro-load-balancing-{}.json", uuid::Uuid::new_v4()));
        std::fs::write(&config_path, r#"{"loadBalancingMode":"priority"}"#).unwrap();

        let config = Config::load(&config_path).unwrap();
        let manager =
            MultiTokenManager::new(config, vec![KiroCredentials::default()], None, None, false)
                .unwrap();

        manager
            .set_load_balancing_mode("balanced".to_string())
            .unwrap();

        let persisted = Config::load(&config_path).unwrap();
        assert_eq!(persisted.load_balancing_mode, "balanced");
        assert_eq!(manager.get_load_balancing_mode(), "balanced");

        std::fs::remove_file(&config_path).unwrap();
    }

    #[tokio::test]
    async fn test_acquire_context_never_reenables_when_all_disabled() {
        // 所有账号都被自动禁用时不自动启用任何账号，只能手动恢复
        let config = Config::default();
        let manager = MultiTokenManager::new(
            config,
            vec![make_valid_cred("t1"), make_valid_cred("t2")],
            None,
            None,
            false,
        )
        .unwrap();

        for _ in 0..MAX_FAILURES_PER_CREDENTIAL {
            manager.report_failure(1, test_error());
            manager.report_refresh_failure(2, test_error());
        }
        assert_eq!(manager.available_count(), 0);

        assert!(manager.acquire_context(None).await.is_err());
        assert_eq!(manager.available_count(), 0);
        let entries = manager.entries.lock();
        assert_eq!(
            entries[0].disabled_reason,
            Some(DisabledReason::TooManyFailures)
        );
        assert_eq!(
            entries[1].disabled_reason,
            Some(DisabledReason::TooManyRefreshFailures)
        );
    }

    #[tokio::test]
    async fn test_unreachable_cools_down_instead_of_disabling() {
        // 请求发送连续失败 3 次：不禁用，冷却期内选号排除，正常完成后清零
        let config = Config::default();
        let manager = MultiTokenManager::new(
            config,
            vec![make_valid_cred("t1"), make_valid_cred("t2")],
            None,
            None,
            false,
        )
        .unwrap();

        for _ in 0..MAX_FAILURES_PER_CREDENTIAL {
            manager.report_unreachable(1, test_error());
        }
        assert_eq!(manager.available_count(), 2, "只冷却，不禁用");

        let snapshot = manager.snapshot();
        let entry = &snapshot.entries[0];
        assert_eq!(entry.health_status, HealthStatus::Unhealthy);
        let cooldown = entry.health_detail.unreachable_cooldown_secs.unwrap();
        assert!(
            (10 * 60 - 5..=10 * 60).contains(&cooldown),
            "首轮冷却 10 分钟，实际 {cooldown}s"
        );
        assert!(entry.last_error.is_some());
        for _ in 0..4 {
            assert_eq!(manager.acquire_context(None).await.unwrap().id, 2);
        }

        manager.report_response_completed(1);
        let snapshot = manager.snapshot();
        assert_eq!(snapshot.entries[0].health_status, HealthStatus::Healthy);
        assert_eq!(
            snapshot.entries[0].health_detail.unreachable_cooldown_secs,
            None
        );
    }

    #[tokio::test]
    async fn test_refresh_success_clears_refresh_failure_count() {
        // 对称于 report_success 清零 failure_count：孤立的偶发刷新失败不应无限累积
        let body = r#"{"access_token":"new-access-token","expires_in":3600}"#;
        let endpoint = spawn_single_response_server(200, body).await;

        let cred1 = KiroCredentials {
            auth_method: Some("external_idp".to_string()),
            refresh_token: Some("short-refresh-token".to_string()),
            client_id: Some("client-id".to_string()),
            token_endpoint: Some(endpoint),
            expires_at: Some((Utc::now() - Duration::hours(1)).to_rfc3339()),
            ..Default::default()
        };

        let config = Config::default();
        let manager = MultiTokenManager::new(config, vec![cred1], None, None, false).unwrap();

        manager.report_refresh_failure(1, test_error());
        manager.report_refresh_failure(1, test_error());
        {
            let entries = manager.entries.lock();
            assert_eq!(entries[0].refresh_failure_count, 2);
        }

        let ctx = manager.acquire_context_filtered(None, &[1]).await.unwrap();
        assert_eq!(ctx.id, 1);

        let entries = manager.entries.lock();
        assert_eq!(
            entries[0].refresh_failure_count, 0,
            "刷新成功后应清零 refresh_failure_count"
        );
    }

    #[tokio::test]
    async fn test_set_disabled_enable_clears_refresh_failure_count() {
        let config = Config::default();
        let mut cred1 = KiroCredentials::default();
        cred1.subscription_title = Some("KIRO PRO".to_string());

        let manager = MultiTokenManager::new(config, vec![cred1], None, None, false).unwrap();

        for _ in 0..MAX_FAILURES_PER_CREDENTIAL {
            manager.report_refresh_failure(1, test_error());
        }
        assert_eq!(manager.available_count(), 0);

        manager.set_disabled(1, false).await.unwrap();

        let entries = manager.entries.lock();
        assert_eq!(entries[0].refresh_failure_count, 0);
        assert!(!entries[0].disabled);
        assert_eq!(entries[0].disabled_reason, None);
    }

    #[tokio::test]
    async fn test_reset_and_enable_clears_refresh_failure_count() {
        let config = Config::default();
        let mut cred1 = KiroCredentials::default();
        cred1.subscription_title = Some("KIRO PRO".to_string());

        let manager = MultiTokenManager::new(config, vec![cred1], None, None, false).unwrap();

        for _ in 0..MAX_FAILURES_PER_CREDENTIAL {
            manager.report_refresh_failure(1, test_error());
        }
        assert_eq!(manager.available_count(), 0);

        manager.reset_and_enable(1).await.unwrap();

        let entries = manager.entries.lock();
        assert_eq!(entries[0].refresh_failure_count, 0);
        assert!(!entries[0].disabled);
        assert_eq!(entries[0].disabled_reason, None);
    }

    #[tokio::test]
    async fn test_set_disabled_enable_lookup_failure_keeps_disabled_state() {
        let mut credential = KiroCredentials {
            disabled: true,
            ..Default::default()
        };
        credential.subscription_title = Some("   ".to_string());
        let manager =
            MultiTokenManager::new(Config::default(), vec![credential], None, None, false).unwrap();

        {
            let mut entries = manager.entries.lock();
            let entry = &mut entries[0];
            entry.failure_count = 2;
            entry.refresh_failure_count = 3;
            entry.disabled_reason = Some(DisabledReason::Manual);
        }

        let error = manager.set_disabled(1, false).await.unwrap_err();
        assert!(error.to_string().contains("账号保持禁用"));

        let entries = manager.entries.lock();
        let entry = &entries[0];
        assert!(entry.disabled);
        assert_eq!(entry.failure_count, 2);
        assert_eq!(entry.refresh_failure_count, 3);
        assert_eq!(entry.disabled_reason, Some(DisabledReason::Manual));
        drop(entries);
        assert_eq!(manager.available_count(), 0);
    }

    #[tokio::test]
    async fn test_reset_and_enable_lookup_failure_keeps_disabled_state() {
        let mut credential = KiroCredentials {
            disabled: true,
            ..Default::default()
        };
        credential.subscription_title = None;
        let manager =
            MultiTokenManager::new(Config::default(), vec![credential], None, None, false).unwrap();

        {
            let mut entries = manager.entries.lock();
            let entry = &mut entries[0];
            entry.failure_count = 2;
            entry.refresh_failure_count = 3;
            entry.disabled_reason = Some(DisabledReason::Manual);
        }

        let error = manager.reset_and_enable(1).await.unwrap_err();
        assert!(error.to_string().contains("账号保持禁用"));

        let entries = manager.entries.lock();
        let entry = &entries[0];
        assert!(entry.disabled);
        assert_eq!(entry.failure_count, 2);
        assert_eq!(entry.refresh_failure_count, 3);
        assert_eq!(entry.disabled_reason, Some(DisabledReason::Manual));
        drop(entries);
        assert_eq!(manager.available_count(), 0);
    }

    #[tokio::test]
    async fn test_set_disabled_waits_for_credential_admin_lock() {
        let mut credential = KiroCredentials {
            disabled: true,
            ..Default::default()
        };
        credential.subscription_title = Some("KIRO PRO".to_string());
        let manager = Arc::new(
            MultiTokenManager::new(Config::default(), vec![credential], None, None, false).unwrap(),
        );

        let guard = manager.credential_admin_lock.lock().await;
        let task = tokio::spawn({
            let manager = Arc::clone(&manager);
            async move { manager.set_disabled(1, false).await }
        });
        tokio::task::yield_now().await;

        assert!(!task.is_finished());
        drop(guard);
        task.await.unwrap().unwrap();
        assert!(!manager.entries.lock()[0].disabled);
    }

    #[test]
    fn test_multi_token_manager_report_quota_exhausted() {
        let config = Config::default();
        let cred1 = KiroCredentials::default();
        let cred2 = KiroCredentials::default();

        let manager =
            MultiTokenManager::new(config, vec![cred1, cred2], None, None, false).unwrap();

        // 账号会自动分配 ID（从 1 开始）
        assert_eq!(manager.available_count(), 2);
        assert!(manager.report_quota_exhausted(1));
        assert_eq!(manager.available_count(), 1);

        // 再禁用第二个后，无可用账号
        assert!(!manager.report_quota_exhausted(2));
        assert_eq!(manager.available_count(), 0);
    }

    #[test]
    fn test_report_refresh_token_invalid_disables_immediately_without_counting() {
        let config = Config::default();
        let cred1 = KiroCredentials::default();
        let cred2 = KiroCredentials::default();

        let manager =
            MultiTokenManager::new(config, vec![cred1, cred2], None, None, false).unwrap();

        assert_eq!(manager.available_count(), 2);
        // 单次调用即立即禁用，不像 report_refresh_failure 需要累计到阈值
        assert!(manager.report_refresh_token_invalid(1, test_error()));
        assert_eq!(manager.available_count(), 1);

        let entries = manager.entries.lock();
        let entry = entries.iter().find(|e| e.id == 1).unwrap();
        assert!(entry.disabled);
        assert_eq!(
            entry.disabled_reason,
            Some(DisabledReason::InvalidRefreshToken)
        );
        assert_eq!(
            entry.refresh_failure_count, 0,
            "invalid_grant 是永久性失效，不应计入 refresh_failure_count"
        );
    }

    #[test]
    fn test_report_refresh_failure_counts_to_threshold_then_disables() {
        let config = Config::default();
        let cred1 = KiroCredentials::default();
        let cred2 = KiroCredentials::default();

        let manager =
            MultiTokenManager::new(config, vec![cred1, cred2], None, None, false).unwrap();

        // 未达阈值前保持可用
        assert!(manager.report_refresh_failure(1, test_error()));
        assert!(manager.report_refresh_failure(1, test_error()));
        assert_eq!(manager.available_count(), 2);

        // 第 3 次达到 MAX_FAILURES_PER_CREDENTIAL 阈值，禁用并设置正确的 disabled_reason
        assert!(manager.report_refresh_failure(1, test_error()));
        assert_eq!(manager.available_count(), 1);

        let entries = manager.entries.lock();
        let entry = entries.iter().find(|e| e.id == 1).unwrap();
        assert!(entry.disabled);
        assert_eq!(
            entry.disabled_reason,
            Some(DisabledReason::TooManyRefreshFailures)
        );
        assert_eq!(entry.refresh_failure_count, MAX_FAILURES_PER_CREDENTIAL);
    }

    #[tokio::test]
    async fn test_multi_token_manager_quota_disabled_is_not_auto_recovered() {
        let config = Config::default();
        let cred1 = KiroCredentials::default();
        let cred2 = KiroCredentials::default();

        let manager =
            MultiTokenManager::new(config, vec![cred1, cred2], None, None, false).unwrap();

        manager.report_quota_exhausted(1);
        manager.report_quota_exhausted(2);
        assert_eq!(manager.available_count(), 0);

        let err = manager
            .acquire_context(None)
            .await
            .err()
            .unwrap()
            .to_string();
        assert!(
            err.contains("本月请求额度已用尽") && err.contains(QUOTA_EXHAUSTED_ALL_MARKER),
            "错误应明确指出额度用尽并带机器可识别标记，实际: {}",
            err
        );
        assert_eq!(manager.available_count(), 0);
    }

    #[tokio::test]
    async fn test_report_failure_preserves_quota_disabled_reason() {
        // 回归：并发下账号已被 report_quota_exhausted 禁用后，
        // 再来一个普通失败（report_failure）不得覆盖 disabled_reason，
        // 否则跨月后不会自动恢复，面板也会显示错误的禁用原因。
        let config = Config::default();
        let mut cred1 = KiroCredentials::default();
        cred1.access_token = Some("t1".to_string());
        cred1.expires_at = Some((Utc::now() + Duration::hours(1)).to_rfc3339());
        let mut cred2 = KiroCredentials::default();
        cred2.access_token = Some("t2".to_string());
        cred2.expires_at = Some((Utc::now() + Duration::hours(1)).to_rfc3339());

        let manager =
            MultiTokenManager::new(config, vec![cred1, cred2], None, None, false).unwrap();

        // 账号 #1 额度用尽被禁用
        manager.report_quota_exhausted(1);
        // 模拟在途的另一请求随后对同一账号报告普通失败
        manager.report_failure(1, test_error());

        // disabled_reason 必须仍是 QuotaExceeded，不能被改写为 TooManyFailures
        {
            let entries = manager.entries.lock();
            let entry = entries.iter().find(|e| e.id == 1).unwrap();
            assert!(entry.disabled);
            assert_eq!(
                entry.disabled_reason,
                Some(DisabledReason::QuotaExceeded),
                "QuotaExceeded 禁用原因被 report_failure 覆盖"
            );
        }

        // 仅 #2 可用，acquire 不应重新启用被额度禁用的 #1
        let ctx = manager.acquire_context(None).await.unwrap();
        assert_eq!(ctx.token, "t2", "额度耗尽账号 #1 不应被重新启用");
        assert_eq!(manager.available_count(), 1);
    }

    // ============ 账号级 Region 优先级测试 ============
}
