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
            .acquire_for_request(model, &[], preferred, avoid, &[])
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

    /// 选号后请求完整产出响应
    async fn serve_keys(
        manager: &MultiTokenManager,
        affinity: &Affinity,
        keys: &RouteKeys,
        avoid: &[u64],
    ) -> (u64, Placement) {
        let routed = route_keys(manager, affinity, keys, None, avoid).await;
        affinity.on_completed(keys, routed.0);
        routed
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

        let (first, _) = serve_keys(&manager, &affinity, &same_prefix("s0"), &[]).await;
        for i in 1..5 {
            let keys = same_prefix(&format!("s{i}"));
            let served = serve_keys(&manager, &affinity, &keys, &[]).await;
            assert_eq!(served, (first, Placement::Preferred));
        }
        assert_eq!(affinity.metrics().prefix_hits, 4);
    }

    #[tokio::test]
    async fn test_failed_spill_keeps_binding() {
        // 本地限额满或本次请求内被限流：换号后请求失败，绑定不变，下次请求回到原账号
        let manager = two_accounts();
        let affinity = Affinity::new();
        let (bound, _) = route(&manager, &affinity, None, &[]).await;

        let (spilled, placement) = route(&manager, &affinity, None, &[bound]).await;
        assert_ne!(spilled, bound);
        assert_eq!(placement, Placement::Spilled);

        assert_eq!(
            route(&manager, &affinity, None, &[]).await,
            (bound, Placement::Preferred)
        );
        assert_eq!(affinity.metrics().rebinds, 0);
    }

    #[tokio::test]
    async fn test_completed_spill_takes_over_binding() {
        // 换号后请求完整产出：新账号缓存了整段前缀，之后留在新账号
        let manager = two_accounts();
        let affinity = Affinity::new();
        let (bound, _) = serve_keys(&manager, &affinity, &session(), &[]).await;

        let (spilled, placement) = serve_keys(&manager, &affinity, &session(), &[bound]).await;
        assert_ne!(spilled, bound);
        assert_eq!(placement, Placement::Spilled);

        assert_eq!(
            route(&manager, &affinity, None, &[]).await,
            (spilled, Placement::Preferred)
        );
        assert_eq!(affinity.metrics().rebinds, 1);
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
    async fn test_upstream_throttle_without_completion_unbinds_after_threshold() {
        // 每个请求先打到绑定账号被 429，同请求内的重试也没产出响应：绑定保持。
        // 连续 THROTTLE_UNBIND_THRESHOLD 次后解绑
        let manager = two_accounts();
        let affinity = Affinity::new();
        let (bound, _) = serve_keys(&manager, &affinity, &session(), &[]).await;

        for _ in 0..THROTTLE_UNBIND_THRESHOLD {
            let (first, _) = route(&manager, &affinity, None, &[]).await;
            assert_eq!(first, bound);
            affinity.on_upstream_throttled(KEY, first);
            route(&manager, &affinity, None, &[first]).await;
        }

        // 最后一次重试发生在解绑之后，已作为新绑定落在重试账号上
        let (next, placement) = route(&manager, &affinity, None, &[]).await;
        assert_ne!(next, bound);
        assert_eq!(placement, Placement::Preferred);
    }

    #[tokio::test]
    async fn test_throttled_binding_moves_to_account_that_completed_retry() {
        let manager = two_accounts();
        let affinity = Affinity::new();
        let (bound, _) = serve_keys(&manager, &affinity, &session(), &[]).await;

        let (first, _) = route(&manager, &affinity, None, &[]).await;
        affinity.on_upstream_throttled(KEY, first);
        let (retry, _) = serve_keys(&manager, &affinity, &session(), &[first]).await;
        assert_ne!(retry, bound);

        assert_eq!(
            route(&manager, &affinity, None, &[]).await,
            (retry, Placement::Preferred)
        );
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

    // ============ 优先级模式同档轮询与 RPM 软上限 ============

    fn cred_with_priority(token: &str, priority: u32) -> KiroCredentials {
        let mut cred = make_valid_cred(token);
        cred.priority = priority;
        cred
    }

    /// priority 模式：账号 1、2 同为最高优先级，账号 3 低一档
    fn tiered(spill_to_lower_priority: bool) -> MultiTokenManager {
        let mut config = Config::default();
        config.soft_rpm_spill_to_lower_priority = spill_to_lower_priority;
        manager(
            vec![
                cred_with_priority("t1", 0),
                cred_with_priority("t2", 0),
                cred_with_priority("t3", 1),
            ],
            config,
        )
    }

    /// 新会话选号若干次，返回选中的账号集合
    async fn picks(
        manager: &MultiTokenManager,
        preferred: Option<u64>,
        soft_full: &[u64],
    ) -> std::collections::BTreeSet<u64> {
        let mut ids = std::collections::BTreeSet::new();
        for _ in 0..6 {
            let (ctx, _) = manager
                .acquire_for_request(None, &[], preferred, &[], soft_full)
                .await
                .unwrap();
            ids.insert(ctx.id);
        }
        ids
    }

    #[tokio::test]
    async fn test_priority_mode_rotates_within_top_tier() {
        let manager = tiered(true);
        let mut ids = std::collections::BTreeSet::new();
        for _ in 0..6 {
            ids.insert(manager.acquire_context(None).await.unwrap().id);
        }
        assert_eq!(ids, [1, 2].into(), "同优先级轮询，不碰低一档");
    }

    #[tokio::test]
    async fn test_soft_full_account_is_skipped_within_tier() {
        for spill in [true, false] {
            assert_eq!(picks(&tiered(spill), None, &[1]).await, [2].into());
        }
    }

    #[tokio::test]
    async fn test_soft_full_top_tier_spills_to_lower_priority() {
        assert_eq!(picks(&tiered(true), None, &[1, 2]).await, [3].into());
    }

    #[tokio::test]
    async fn test_soft_full_top_tier_stays_when_spill_disabled() {
        assert_eq!(picks(&tiered(false), None, &[1, 2]).await, [1, 2].into());
    }

    #[tokio::test]
    async fn test_all_soft_full_still_selects_by_priority() {
        assert_eq!(picks(&tiered(true), None, &[1, 2, 3]).await, [1, 2].into());
    }

    #[tokio::test]
    async fn test_soft_full_family_account_is_not_followed() {
        let manager = tiered(true);
        let (ctx, placement) = manager
            .acquire_for_request(None, &[], Some(1), &[], &[1])
            .await
            .unwrap();
        assert_eq!(ctx.id, 2);
        assert_eq!(placement, Placement::Fresh);
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
