// Copyright (c) 2026 Harllan He. Licensed under MIT.
// token_manager 测试（自 tests.rs 拆出，纯代码搬移）
#[cfg(test)]
pub(crate) mod tests {

    use super::super::super::refresh::{
        UsageLimitsHttpError, extract_profile_arns, is_invalid_grant_response, kam_style_region,
        kiro_ide_user_agents, pick_profile_arn, send_list_profiles, sha256_hex,
        should_try_next_region, validate_refresh_token, with_kiro_ide_headers,
    };
    use super::super::super::types::MultiTokenManager;
    use crate::kiro::token_manager::tests::ext_idp::tests::spawn_single_response_server;

    use crate::kiro::model::credentials::KiroCredentials;

    #[test]
    fn test_validate_refresh_token_missing() {
        let credentials = KiroCredentials::default();
        let result = validate_refresh_token(&credentials);
        assert!(result.is_err());
    }

    #[test]
    fn test_validate_refresh_token_valid() {
        let mut credentials = KiroCredentials::default();
        credentials.refresh_token = Some("a".repeat(150));
        let result = validate_refresh_token(&credentials);
        assert!(result.is_ok());
    }

    #[test]
    fn test_sha256_hex() {
        let result = sha256_hex("test");
        assert_eq!(
            result,
            "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
        );
    }

    #[test]
    fn test_is_invalid_grant_response_matches_400_with_expected_body() {
        let body =
            r#"{"error":"invalid_grant","error_description":"Invalid refresh token provided"}"#;
        assert!(is_invalid_grant_response(400, body));
    }

    #[test]
    fn test_is_invalid_grant_response_rejects_other_body() {
        let body = r#"{"error":"server_error","error_description":"something else"}"#;
        assert!(!is_invalid_grant_response(400, body));
    }

    #[test]
    fn test_is_invalid_grant_response_rejects_non_400_status() {
        let body =
            r#"{"error":"invalid_grant","error_description":"Invalid refresh token provided"}"#;
        assert!(!is_invalid_grant_response(401, body));
    }

    #[test]
    fn test_apply_update_fields_clears_subscription_for_identity_updates() {
        let updates = [
            crate::admin::types::UpdateCredentialRequest {
                refresh_token: Some("new-refresh-token".to_string()),
                ..Default::default()
            },
            crate::admin::types::UpdateCredentialRequest {
                auth_method: Some("idc".to_string()),
                ..Default::default()
            },
            crate::admin::types::UpdateCredentialRequest {
                client_id: Some("new-client-id".to_string()),
                ..Default::default()
            },
            crate::admin::types::UpdateCredentialRequest {
                client_secret: Some("new-client-secret".to_string()),
                ..Default::default()
            },
            crate::admin::types::UpdateCredentialRequest {
                profile_arn: Some("new-profile-arn".to_string()),
                ..Default::default()
            },
        ];

        for update in updates {
            let mut credentials = KiroCredentials {
                subscription_title: Some("KIRO PRO".to_string()),
                ..Default::default()
            };
            MultiTokenManager::apply_update_fields(&mut credentials, &update);
            assert_eq!(credentials.subscription_title, None);
        }
    }

    #[test]
    fn test_apply_update_fields_preserves_subscription_for_non_identity_updates() {
        let mut credentials = KiroCredentials {
            subscription_title: Some("KIRO PRO".to_string()),
            ..Default::default()
        };
        let update = crate::admin::types::UpdateCredentialRequest {
            nickname: Some("new-name".to_string()),
            // 登录来源是纯展示字段，不参与认证流程与订阅等级判定，变更后订阅信息仍然有效
            provider: Some("Google".to_string()),
            ..Default::default()
        };

        MultiTokenManager::apply_update_fields(&mut credentials, &update);

        assert_eq!(credentials.subscription_title.as_deref(), Some("KIRO PRO"));
    }

    #[test]
    fn test_stale_subscription_title_cannot_overwrite_new_identity() {
        let queried_credentials = KiroCredentials {
            refresh_token: Some("old-refresh-token".to_string()),
            subscription_title: None,
            ..Default::default()
        };
        let manager = MultiTokenManager::new(
            crate::model::config::Config::default(),
            vec![queried_credentials.clone()],
            None,
            None,
            false,
        )
        .unwrap();
        manager.entries.lock()[0].credentials.refresh_token = Some("new-refresh-token".to_string());

        let error = manager
            .commit_subscription_title_if_identity_matches(
                1,
                &queried_credentials,
                Some("KIRO PRO"),
            )
            .unwrap_err();

        assert!(error.to_string().contains("身份已变更"));
        assert_eq!(
            manager.entries.lock()[0].credentials.subscription_title,
            None
        );
    }

    #[test]
    fn test_current_subscription_title_is_committed() {
        let initial_credentials = KiroCredentials {
            refresh_token: Some("current-refresh-token".to_string()),
            subscription_title: None,
            ..Default::default()
        };
        let manager = MultiTokenManager::new(
            crate::model::config::Config::default(),
            vec![initial_credentials],
            None,
            None,
            false,
        )
        .unwrap();
        let queried_credentials = manager.entries.lock()[0].credentials.clone();

        let changed = manager
            .commit_subscription_title_if_identity_matches(
                1,
                &queried_credentials,
                Some("KIRO PRO"),
            )
            .unwrap();

        assert!(changed);
        assert_eq!(
            manager.entries.lock()[0]
                .credentials
                .subscription_title
                .as_deref(),
            Some("KIRO PRO")
        );
    }

    #[test]
    fn test_unknown_subscription_clears_stale_pro_title() {
        let initial_credentials = KiroCredentials {
            refresh_token: Some("current-refresh-token".to_string()),
            subscription_title: Some("KIRO PRO".to_string()),
            ..Default::default()
        };
        let manager = MultiTokenManager::new(
            crate::model::config::Config::default(),
            vec![initial_credentials],
            None,
            None,
            false,
        )
        .unwrap();
        let queried_credentials = manager.entries.lock()[0].credentials.clone();

        let changed = manager
            .commit_subscription_title_if_identity_matches(1, &queried_credentials, None)
            .unwrap();

        assert!(changed);
        assert_eq!(
            manager.entries.lock()[0].credentials.subscription_title,
            None
        );
    }

    #[test]
    fn test_stale_token_refresh_cannot_overwrite_new_identity() {
        let initial_credentials = KiroCredentials {
            refresh_token: Some("old-refresh-token".to_string()),
            client_id: Some("old-client-id".to_string()),
            access_token: Some("old-access-token".to_string()),
            ..Default::default()
        };
        let manager = MultiTokenManager::new(
            crate::model::config::Config::default(),
            vec![initial_credentials],
            None,
            None,
            false,
        )
        .unwrap();
        let refreshing_credentials = manager.entries.lock()[0].credentials.clone();
        {
            let mut entries = manager.entries.lock();
            entries[0].credentials.refresh_token = Some("new-refresh-token".to_string());
            entries[0].credentials.client_id = Some("new-client-id".to_string());
            entries[0].credentials.access_token = Some("new-access-token".to_string());
        }
        let mut refreshed_credentials = refreshing_credentials.clone();
        refreshed_credentials.access_token = Some("stale-refreshed-access-token".to_string());
        refreshed_credentials.refresh_token = Some("stale-rotated-refresh-token".to_string());

        let error = manager
            .commit_refreshed_credentials_if_identity_matches(
                1,
                &refreshing_credentials,
                &refreshed_credentials,
            )
            .unwrap_err();

        assert!(error.to_string().contains("身份已变更"));
        let credentials = &manager.entries.lock()[0].credentials;
        assert_eq!(
            credentials.refresh_token.as_deref(),
            Some("new-refresh-token")
        );
        assert_eq!(credentials.client_id.as_deref(), Some("new-client-id"));
        assert_eq!(
            credentials.access_token.as_deref(),
            Some("new-access-token")
        );
    }

    #[test]
    fn test_token_refresh_preserves_concurrent_non_identity_updates() {
        let initial_credentials = KiroCredentials {
            refresh_token: Some("current-refresh-token".to_string()),
            access_token: Some("old-access-token".to_string()),
            nickname: Some("old-name".to_string()),
            subscription_title: Some("KIRO PRO".to_string()),
            ..Default::default()
        };
        let manager = MultiTokenManager::new(
            crate::model::config::Config::default(),
            vec![initial_credentials],
            None,
            None,
            false,
        )
        .unwrap();
        let refreshing_credentials = manager.entries.lock()[0].credentials.clone();
        {
            let mut entries = manager.entries.lock();
            let credentials = &mut entries[0].credentials;
            credentials.nickname = Some("new-name".to_string());
            credentials.email = Some("fictional@example.invalid".to_string());
            credentials.proxy_url = Some("direct".to_string());
            credentials.priority = 7;
            credentials.subscription_title = Some("KIRO FREE".to_string());
        }
        let mut refreshed_credentials = refreshing_credentials.clone();
        refreshed_credentials.access_token = Some("refreshed-access-token".to_string());
        refreshed_credentials.sso_access_token = Some("refreshed-sso-token".to_string());
        refreshed_credentials.refresh_token = Some("rotated-refresh-token".to_string());
        refreshed_credentials.profile_arn = Some("refreshed-profile-arn".to_string());
        refreshed_credentials.expires_at = Some("2099-01-01T00:00:00Z".to_string());

        let committed = manager
            .commit_refreshed_credentials_if_identity_matches(
                1,
                &refreshing_credentials,
                &refreshed_credentials,
            )
            .unwrap();

        assert_eq!(
            committed.access_token.as_deref(),
            Some("refreshed-access-token")
        );
        assert_eq!(
            committed.sso_access_token.as_deref(),
            Some("refreshed-sso-token")
        );
        assert_eq!(
            committed.refresh_token.as_deref(),
            Some("rotated-refresh-token")
        );
        assert_eq!(
            committed.profile_arn.as_deref(),
            Some("refreshed-profile-arn")
        );
        assert_eq!(
            committed.expires_at.as_deref(),
            Some("2099-01-01T00:00:00Z")
        );
        assert_eq!(committed.nickname.as_deref(), Some("new-name"));
        assert_eq!(
            committed.email.as_deref(),
            Some("fictional@example.invalid")
        );
        assert_eq!(committed.proxy_url.as_deref(), Some("direct"));
        assert_eq!(committed.priority, 7);
        assert_eq!(committed.subscription_title.as_deref(), Some("KIRO FREE"));
    }

    #[tokio::test]
    async fn test_identity_update_waits_for_refresh_identity_lock() {
        let manager = std::sync::Arc::new(
            MultiTokenManager::new(
                crate::model::config::Config::default(),
                vec![KiroCredentials {
                    refresh_token: Some("current-refresh-token".to_string()),
                    machine_id: Some("old-machine-id".to_string()),
                    ..Default::default()
                }],
                None,
                None,
                false,
            )
            .unwrap(),
        );
        let identity_guard = manager.credential_identity_lock.lock().await;
        let update_manager = std::sync::Arc::clone(&manager);
        let update_task = tokio::spawn(async move {
            update_manager
                .update_credential(
                    1,
                    crate::admin::types::UpdateCredentialRequest {
                        machine_id: Some("new-machine-id".to_string()),
                        ..Default::default()
                    },
                )
                .await
        });

        tokio::task::yield_now().await;
        assert!(!update_task.is_finished());

        drop(identity_guard);
        tokio::time::timeout(std::time::Duration::from_secs(1), update_task)
            .await
            .expect("身份锁释放后更新应及时完成")
            .expect("更新任务不应 panic")
            .expect("身份更新应成功");
        assert_eq!(
            manager.entries.lock()[0].credentials.machine_id.as_deref(),
            Some("new-machine-id")
        );
    }

    #[tokio::test]
    async fn test_discovered_profile_arn_commit_rejects_stale_identity() {
        // 写回 profileArn 仅在账号身份与查询时快照一致时生效；
        // 查询期间 refreshToken 被更换（身份已变更）时必须拒绝脏写
        let manager = std::sync::Arc::new(
            MultiTokenManager::new(
                crate::model::config::Config::default(),
                vec![KiroCredentials {
                    refresh_token: Some("current-refresh-token".to_string()),
                    ..Default::default()
                }],
                None,
                None,
                false,
            )
            .unwrap(),
        );
        let credentials = manager.entries.lock()[0].credentials.clone();

        // 1) 身份一致 → 写回成功
        manager
            .commit_discovered_profile_arn(
                1,
                &credentials,
                "arn:aws:codewhisperer:us-east-1:111122223333:profile/ABCDEFGHIJKL",
            )
            .expect("身份一致时写回应成功");
        assert_eq!(
            manager.entries.lock()[0].credentials.profile_arn.as_deref(),
            Some("arn:aws:codewhisperer:us-east-1:111122223333:profile/ABCDEFGHIJKL")
        );

        // 2) 查询快照过期（refreshToken 已更换）→ 拒绝写回，内存中 ARN 不变
        let mut stale = credentials.clone();
        stale.refresh_token = Some("rotated-refresh-token".to_string());
        let err = manager
            .commit_discovered_profile_arn(
                1,
                &stale,
                "arn:aws:codewhisperer:us-east-1:111122223333:profile/ZZZZZZZZZZZZ",
            )
            .expect_err("身份已变更时写回应被拒绝");
        assert!(err.to_string().contains("身份已变更"));
        assert_eq!(
            manager.entries.lock()[0].credentials.profile_arn.as_deref(),
            Some("arn:aws:codewhisperer:us-east-1:111122223333:profile/ABCDEFGHIJKL"),
            "拒绝后不应覆盖已写回的 ARN"
        );

        // 3) 账号不存在 → 报错
        assert!(
            manager
                .commit_discovered_profile_arn(
                    999,
                    &credentials,
                    "arn:aws:codewhisperer:us-east-1:111122223333:profile/AAAAAAAAAAAA",
                )
                .is_err()
        );
    }

    #[test]
    fn test_apply_update_fields_provider() {
        let mut cred = KiroCredentials::default();

        // Some(非空) 写入登录来源
        let update = crate::admin::types::UpdateCredentialRequest {
            provider: Some("Google".to_string()),
            ..Default::default()
        };
        MultiTokenManager::apply_update_fields(&mut cred, &update);
        assert_eq!(cred.provider.as_deref(), Some("Google"));

        // 空字符串清除该字段
        let update = crate::admin::types::UpdateCredentialRequest {
            provider: Some(String::new()),
            ..Default::default()
        };
        MultiTokenManager::apply_update_fields(&mut cred, &update);
        assert_eq!(cred.provider, None);

        // None 表示不更新该字段
        cred.provider = Some("GitHub".to_string());
        let update = crate::admin::types::UpdateCredentialRequest {
            provider: None,
            ..Default::default()
        };
        MultiTokenManager::apply_update_fields(&mut cred, &update);
        assert_eq!(cred.provider.as_deref(), Some("GitHub"));
    }

    #[test]
    fn test_extract_profile_arns_supports_arn_and_profile_arn_fields() {
        let v = serde_json::json!({
            "profiles": [
                {"arn": "arn:aws:codewhisperer:us-east-1:111:profile/A", "profileName": "a"},
                {"profileArn": "arn:aws:codewhisperer:eu-central-1:111:profile/B"},
                {"arn": ""},
                {"name": "no-arn"}
            ]
        });
        assert_eq!(
            extract_profile_arns(&v),
            vec![
                "arn:aws:codewhisperer:us-east-1:111:profile/A".to_string(),
                "arn:aws:codewhisperer:eu-central-1:111:profile/B".to_string(),
            ]
        );
        assert!(extract_profile_arns(&serde_json::json!({})).is_empty());
        assert!(extract_profile_arns(&serde_json::json!({"profiles": null})).is_empty());
    }

    #[test]
    fn test_pick_profile_arn_prefers_region_match_then_first() {
        let arns = vec![
            "arn:aws:codewhisperer:us-east-1:111:profile/A".to_string(),
            "arn:aws:codewhisperer:eu-central-1:111:profile/B".to_string(),
        ];
        assert_eq!(
            pick_profile_arn(&arns, "eu-central-1").as_deref(),
            Some("arn:aws:codewhisperer:eu-central-1:111:profile/B")
        );
        // 无区域匹配 → 第一个
        assert_eq!(
            pick_profile_arn(&arns, "ap-southeast-1").as_deref(),
            Some("arn:aws:codewhisperer:us-east-1:111:profile/A")
        );
        assert_eq!(pick_profile_arn(&[], "us-east-1"), None);
    }

    #[test]
    fn test_kam_style_region_maps_to_supported_control_plane_regions() {
        assert_eq!(kam_style_region("us-east-1"), "us-east-1");
        assert_eq!(kam_style_region("us-west-2"), "us-east-1");
        assert_eq!(kam_style_region("eu-central-1"), "eu-central-1");
        assert_eq!(kam_style_region("EU-WEST-1"), "eu-central-1");
    }

    #[test]
    fn test_kiro_ide_headers_are_identical_for_both_list_profiles_shapes() {
        // ListAvailableProfiles 的 REST / JSON-RPC 两种形态必须带同一套 Kiro IDE 请求头，
        // 否则一种形态被上游按指纹拦截时无法与另一种对比排查
        let credentials = KiroCredentials::default();
        let config = crate::model::config::Config::default();
        let agents = kiro_ide_user_agents(&credentials, &config);
        assert!(agents.user_agent.contains("KiroIDE-"));
        assert!(agents.amz_user_agent.contains("KiroIDE-"));

        let client = reqwest::Client::new();
        for url in [
            "https://example.com/ListAvailableProfiles",
            "https://example.com/",
        ] {
            let req = with_kiro_ide_headers(client.post(url), &agents)
                .build()
                .unwrap();
            let h = req.headers();
            assert_eq!(h["user-agent"].to_str().unwrap(), agents.user_agent);
            assert_eq!(
                h["x-amz-user-agent"].to_str().unwrap(),
                agents.amz_user_agent
            );
            assert!(h.contains_key("amz-sdk-invocation-id"));
            assert_eq!(h["amz-sdk-request"], "attempt=1; max=1");
        }
    }

    #[tokio::test]
    async fn test_send_list_profiles_parses_arns_on_success() {
        let url = spawn_single_response_server(
            200,
            r#"{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:111122223333:profile/ABCDEFGHIJKL"}]}"#,
        )
        .await;
        let arns = send_list_profiles(reqwest::Client::new().post(url))
            .await
            .unwrap();
        assert_eq!(
            arns,
            vec!["arn:aws:codewhisperer:us-east-1:111122223333:profile/ABCDEFGHIJKL"]
        );
    }

    #[tokio::test]
    async fn test_send_list_profiles_reports_status_on_http_error() {
        let url = spawn_single_response_server(403, r#"{"message":"denied"}"#).await;
        let err = send_list_profiles(reqwest::Client::new().post(url))
            .await
            .unwrap_err();
        assert!(err.contains("403") && err.contains("denied"), "实际: {err}");
    }

    #[tokio::test]
    async fn test_send_list_profiles_reports_unparseable_body_as_error() {
        // 2xx 但响应体不是 JSON：应作为可重试的 Err 返回，让调用方换另一种请求形态
        let url = spawn_single_response_server(200, "not json").await;
        let err = send_list_profiles(reqwest::Client::new().post(url))
            .await
            .unwrap_err();
        assert!(err.contains("响应解析失败"), "实际: {err}");
    }

    fn http_error(code: u16, body: &str) -> anyhow::Error {
        UsageLimitsHttpError::from_response(
            reqwest::StatusCode::from_u16(code).unwrap(),
            body.to_string(),
        )
        .into()
    }

    #[test]
    fn test_usage_limits_http_error_display_keeps_legacy_message_format() {
        // admin/service.rs 的错误分类依赖这些关键字，格式变动会让其静默失效
        assert_eq!(
            http_error(403, "denied").to_string(),
            "权限不足，无法获取使用额度: 403 Forbidden denied"
        );
        assert!(
            http_error(401, "")
                .to_string()
                .starts_with("认证失败，Token 无效或已过期: 401")
        );
        assert!(
            http_error(429, "")
                .to_string()
                .starts_with("请求过于频繁，已被限流: 429")
        );
        assert!(
            http_error(503, "")
                .to_string()
                .starts_with("服务器错误，AWS 服务暂时不可用: 503")
        );
        assert!(
            http_error(418, "")
                .to_string()
                .starts_with("获取使用额度失败: 418")
        );
    }

    #[test]
    fn test_should_try_next_region_only_on_403_status() {
        assert!(should_try_next_region(&http_error(403, "")));
        for code in [400, 401, 429, 500, 503] {
            assert!(!should_try_next_region(&http_error(code, "")), "{code}");
        }
        // 非 HTTP 状态错误（网络等）不换区域；不依赖文案：含“权限不足”字样的普通错误同样不触发
        assert!(!should_try_next_region(&anyhow::anyhow!("权限不足")));
    }
}
