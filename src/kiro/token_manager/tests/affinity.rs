// Copyright (c) 2026 Harllan He. Licensed under MIT.
// 会话亲和选号：acquire_for_request + Affinity 联动（按 provider 重试循环的调用方式）
#[cfg(test)]
pub(crate) mod tests {

    use super::super::super::types::{MultiTokenManager, Placement};

    use crate::kiro::affinity::{Affinity, RouteKeys, THROTTLE_UNBIND_THRESHOLD};
    use crate::kiro::model::credentials::KiroCredentials;

    use crate::model::config::Config;
    use chrono::{Duration, Utc};

    const KEY: &str = "conversation-1";

    pub(crate) fn make_valid_cred(token: &str) -> KiroCredentials {
        let mut c = KiroCredentials::default();
        c.access_token = Some(token.to_string());
        c.expires_at = Some((Utc::now() + Duration::hours(1)).to_rfc3339());
        c
    }

    fn manager(creds: Vec<KiroCredentials>, config: Config) -> MultiTokenManager {
        MultiTokenManager::new(config, creds, None, None, false).unwrap()
    }

    fn two_accounts() -> MultiTokenManager {
        manager(
            vec![make_valid_cred("t1"), make_valid_cred("t2")],
            Config::default(),
        )
    }

    /// 一次选号，与 provider 重试循环相同：取偏好 → 选号 → 记录落点
    async fn route_keys(
        manager: &MultiTokenManager,
        affinity: &Affinity,
        keys: &RouteKeys,
        model: Option<&str>,
        avoid: &[u64],
    ) -> (u64, Placement) {
        let preferred = affinity.preferred(keys);
        let (ctx, placement) = manager
            .acquire_for_request(model, &[], preferred, avoid)
            .await
            .unwrap();
        affinity.on_routed(keys, ctx.id, placement);
        (ctx.id, placement)
    }

    /// 会话 KEY，无前缀族
    fn session() -> RouteKeys {
        RouteKeys {
            conversation: KEY.to_string(),
            prefix_family: None,
        }
    }

    async fn route(
        manager: &MultiTokenManager,
        affinity: &Affinity,
        model: Option<&str>,
        avoid: &[u64],
    ) -> (u64, Placement) {
        route_keys(manager, affinity, &session(), model, avoid).await
    }

    #[tokio::test]
    async fn test_first_request_binds_and_next_request_prefers_it() {
        let manager = two_accounts();
        let affinity = Affinity::new();

        let (bound, placement) = route(&manager, &affinity, None, &[]).await;
        assert_eq!(placement, Placement::Fresh);

        for _ in 0..3 {
            assert_eq!(
                route(&manager, &affinity, None, &[]).await,
                (bound, Placement::Preferred)
            );
        }
        assert_eq!(affinity.metrics().preferred, 3);
    }

    #[tokio::test]
    async fn test_balanced_mode_keeps_binding() {
        let mut config = Config::default();
        config.load_balancing_mode = "balanced".to_string();
        let manager = manager(vec![make_valid_cred("t1"), make_valid_cred("t2")], config);
        let affinity = Affinity::new();

        // balanced 模式无偏好时会轮转，有绑定时应固定在绑定账号
        let (bound, _) = route(&manager, &affinity, None, &[]).await;
        for _ in 0..5 {
            assert_eq!(route(&manager, &affinity, None, &[]).await.0, bound);
        }
    }

    #[tokio::test]
    async fn test_new_sessions_follow_prefix_family_in_balanced_mode() {
        // balanced 模式无偏好时轮转；同前缀的新会话应落到已缓存该前缀的账号
        let mut config = Config::default();
        config.load_balancing_mode = "balanced".to_string();
        let manager = manager(vec![make_valid_cred("t1"), make_valid_cred("t2")], config);
        let affinity = Affinity::new();
        let same_prefix = |name: &str| RouteKeys {
            conversation: name.to_string(),
            prefix_family: Some("family".to_string()),
        };

        let (first, _) = route_keys(&manager, &affinity, &same_prefix("s0"), None, &[]).await;
        for i in 1..5 {
            let keys = same_prefix(&format!("s{i}"));
            let routed = route_keys(&manager, &affinity, &keys, None, &[]).await;
            assert_eq!(routed, (first, Placement::Preferred));
        }
        assert_eq!(affinity.metrics().prefix_hits, 4);
    }

    #[tokio::test]
    async fn test_avoided_preferred_spills_without_touching_binding() {
        // 本地限额满或本次请求内被限流：本次换号完成，绑定不变，下次请求回到原账号
        let manager = two_accounts();
        let affinity = Affinity::new();
        let (bound, _) = route(&manager, &affinity, None, &[]).await;

        let (spilled, placement) = route(&manager, &affinity, None, &[bound]).await;
        assert_ne!(spilled, bound);
        assert_eq!(placement, Placement::Spilled);
        assert_eq!(affinity.preferred(&session()), Some(bound));

        assert_eq!(
            route(&manager, &affinity, None, &[]).await,
            (bound, Placement::Preferred)
        );
        assert_eq!(affinity.metrics().rebinds, 0);
    }

    #[tokio::test]
    async fn test_avoiding_every_account_falls_back_to_preferred() {
        // 候选全被避让时不失败，退回原选择逻辑（由上层退避）
        let manager = manager(vec![make_valid_cred("t1")], Config::default());
        let affinity = Affinity::new();
        let (bound, _) = route(&manager, &affinity, None, &[]).await;

        assert_eq!(
            route(&manager, &affinity, None, &[bound]).await,
            (bound, Placement::Preferred)
        );
    }

    #[tokio::test]
    async fn test_upstream_throttle_once_per_request_unbinds_after_threshold() {
        // 每个请求先打到绑定账号被 429 一次，同请求内重试溢出到别的账号成功。
        // 连续 THROTTLE_UNBIND_THRESHOLD 个请求如此应解绑，下一个请求重新绑定
        let manager = two_accounts();
        let affinity = Affinity::new();
        let (bound, _) = route(&manager, &affinity, None, &[]).await;

        for _ in 0..THROTTLE_UNBIND_THRESHOLD {
            let (first, _) = route(&manager, &affinity, None, &[]).await;
            assert_eq!(first, bound);
            affinity.on_upstream_throttled(KEY, first);

            let (retry, _) = route(&manager, &affinity, None, &[first]).await;
            affinity.on_completed(KEY, retry);
        }

        // 最后一次溢出发生在解绑之后，已作为新绑定落在溢出账号上
        let (next, placement) = route(&manager, &affinity, None, &[]).await;
        assert_ne!(next, bound);
        assert_eq!(placement, Placement::Preferred);
    }

    #[tokio::test]
    async fn test_disabled_bound_account_rebinds() {
        let manager = two_accounts();
        let affinity = Affinity::new();
        let (bound, _) = route(&manager, &affinity, None, &[]).await;

        manager.report_quota_exhausted(bound);

        let (next, placement) = route(&manager, &affinity, None, &[]).await;
        assert_ne!(next, bound);
        assert_eq!(placement, Placement::Fresh);
        assert_eq!(affinity.preferred(&session()), Some(next));
        assert_eq!(affinity.metrics().rebinds, 1);
    }

    #[tokio::test]
    async fn test_opus_request_rebinds_away_from_free_account() {
        let mut free = make_valid_cred("free-token");
        free.priority = 0;
        free.subscription_title = Some("KIRO FREE".to_string());

        let mut pro = make_valid_cred("pro-token");
        pro.priority = 1;
        pro.subscription_title = Some("KIRO PRO".to_string());

        let manager = manager(vec![free, pro], Config::default());
        let affinity = Affinity::new();

        let (initial, _) = route(&manager, &affinity, Some("claude-sonnet-4-6"), &[]).await;
        assert_eq!(initial, 1);

        let (opus, placement) = route(&manager, &affinity, Some("claude-opus-4-7"), &[]).await;
        assert_eq!(opus, 2);
        assert_eq!(placement, Placement::Fresh);
        assert_eq!(affinity.preferred(&session()), Some(2));
        assert_eq!(affinity.metrics().rebinds, 1);
    }

    fn make_unhealthy(manager: &MultiTokenManager, id: u64) {
        for _ in 0..3 {
            manager.report_throttled(id);
        }
    }

    #[tokio::test]
    async fn test_unhealthy_bound_account_rebinds_to_healthy_one() {
        let manager = two_accounts();
        let affinity = Affinity::new();
        let (bound, _) = route(&manager, &affinity, None, &[]).await;

        make_unhealthy(&manager, bound);

        let (next, placement) = route(&manager, &affinity, None, &[]).await;
        assert_ne!(next, bound);
        assert_eq!(placement, Placement::Fresh);
    }

    #[tokio::test]
    async fn test_all_accounts_unhealthy_keeps_binding() {
        // 候选全 Unhealthy 时退回全集：绑定账号仍在候选里，不应来回改绑
        let manager = two_accounts();
        let affinity = Affinity::new();
        let (bound, _) = route(&manager, &affinity, None, &[]).await;

        for id in manager.credential_ids() {
            make_unhealthy(&manager, id);
        }

        for _ in 0..3 {
            assert_eq!(
                route(&manager, &affinity, None, &[]).await,
                (bound, Placement::Preferred)
            );
        }
        assert_eq!(affinity.metrics().rebinds, 0);
    }
}
