// Copyright (c) 2026 Harllan He. Licensed under MIT.
// 健康度判定（近期上游失败）、冷却中的试探、冷却后的待验证与降级账号的选号顺位
#[cfg(test)]
mod tests {
    use super::super::super::entry::{DisabledReason, HealthStatus};
    use super::super::super::probe::AccountsCoolingDown;
    use super::super::super::types::MultiTokenManager;
    use super::super::super::upstream_failures::UpstreamFailure;
    use super::super::affinity::tests::make_valid_cred;
    use super::super::multi::tests::test_error;
    use crate::model::config::Config;
    use std::time::{Duration, Instant};

    const MINUTE: Duration = Duration::from_secs(60);

    fn secs(n: u64) -> Duration {
        Duration::from_secs(n)
    }

    /// 按给定优先级建账号，id 依次为 1, 2, …
    fn manager(priorities: &[u32]) -> MultiTokenManager {
        let creds = priorities
            .iter()
            .enumerate()
            .map(|(i, &priority)| {
                let mut c = make_valid_cred(&format!("t{i}"));
                c.priority = priority;
                c
            })
            .collect();
        MultiTokenManager::new(Config::default(), creds, None, None, false).unwrap()
    }

    fn health_at(m: &MultiTokenManager, id: u64, at: Instant) -> HealthStatus {
        let entries = m.entries.lock();
        let entry = entries.iter().find(|e| e.id == id).unwrap();
        MultiTokenManager::compute_health_at(entry, at)
    }

    fn health(m: &MultiTokenManager, id: u64) -> HealthStatus {
        health_at(m, id, Instant::now())
    }

    /// 零散失败：每次失败后跟一次正常完成，连续计数保持为 1
    fn scattered_failures(m: &MultiTokenManager, id: u64, n: usize) {
        for _ in 0..n {
            m.report_throttled(id);
            m.report_response_completed(id, None);
        }
    }

    #[test]
    fn long_success_history_does_not_dilute_recent_throttles() {
        let m = manager(&[0]);
        m.entries.lock()[0].success_count = 10_000;
        for _ in 0..3 {
            m.report_throttled(1);
        }
        assert_eq!(health(&m, 1), HealthStatus::Unhealthy);
    }

    #[test]
    fn scattered_failure_thresholds() {
        let m = manager(&[0]);
        scattered_failures(&m, 1, 1);
        assert_eq!(health(&m, 1), HealthStatus::Healthy, "1 次不报警");
        scattered_failures(&m, 1, 1);
        assert_eq!(health(&m, 1), HealthStatus::Warning, "F=2");
        scattered_failures(&m, 1, 7);
        assert_eq!(health(&m, 1), HealthStatus::Warning, "F=9");
        scattered_failures(&m, 1, 1);
        assert_eq!(health(&m, 1), HealthStatus::Degraded, "F=10");
        scattered_failures(&m, 1, 9);
        assert_eq!(health(&m, 1), HealthStatus::Degraded, "F=19");
        scattered_failures(&m, 1, 1);
        assert_eq!(health(&m, 1), HealthStatus::Unhealthy, "F=20");
    }

    #[test]
    fn consecutive_failures_have_lower_thresholds() {
        let m = manager(&[0]);
        m.report_throttled(1);
        m.report_abnormal_response(1, test_error());
        assert_eq!(health(&m, 1), HealthStatus::Degraded, "C=2");
        m.report_throttled(1);
        assert_eq!(health(&m, 1), HealthStatus::Unhealthy, "C=3");
    }

    #[test]
    fn unhealthy_only_during_cooldown_then_pending_verification() {
        let m = manager(&[0]);
        for _ in 0..3 {
            m.report_throttled(1);
        }
        let now = Instant::now();
        assert_eq!(health_at(&m, 1, now), HealthStatus::Unhealthy);
        // 冷却 2 分钟后放回选号，等一次成功响应；离开冷却后再失败一次，冷却翻倍到 4 分钟
        let after_cooldown = now + 2 * MINUTE + secs(1);
        assert_eq!(
            health_at(&m, 1, after_cooldown),
            HealthStatus::PendingVerification
        );
        m.entries.lock()[0]
            .upstream_failures
            .record(UpstreamFailure::Throttled, after_cooldown);
        assert_eq!(
            health_at(&m, 1, after_cooldown + 3 * MINUTE),
            HealthStatus::Unhealthy
        );
        assert_eq!(
            health_at(&m, 1, after_cooldown + 4 * MINUTE + secs(1)),
            HealthStatus::PendingVerification
        );
    }

    #[test]
    fn completed_response_ends_streak_and_idle_account_recovers() {
        let m = manager(&[0]);
        for _ in 0..3 {
            m.report_throttled(1);
        }
        m.report_response_completed(1, None);
        // 连续中断，但窗口内仍有 3 次
        assert_eq!(health(&m, 1), HealthStatus::Warning);

        let m = manager(&[0]);
        for _ in 0..3 {
            m.report_throttled(1);
        }
        // 空闲 10 分钟：窗口与连续计数都过期，但没有一次完整成功的响应，仍待验证
        assert_eq!(
            health_at(&m, 1, Instant::now() + 10 * MINUTE),
            HealthStatus::PendingVerification
        );
        m.report_response_completed(1, None);
        assert_eq!(
            health_at(&m, 1, Instant::now() + 10 * MINUTE),
            HealthStatus::Healthy
        );
    }

    /// 200 响应头到达时的 report_success 不中断连续失败：异常结束要到读完才知道
    #[test]
    fn abnormal_responses_after_header_success_still_accumulate() {
        let m = manager(&[0]);
        for _ in 0..2 {
            m.report_success(1);
            m.report_abnormal_response(1, test_error());
        }
        assert_eq!(health(&m, 1), HealthStatus::Degraded);
        let snapshot = m.snapshot();
        assert_eq!(
            snapshot.entries[0]
                .last_error
                .as_ref()
                .map(|e| e.message.as_str()),
            Some("test failure")
        );
        let detail = snapshot.entries[0].health_detail;
        assert_eq!(detail.recent_abnormal_count, 2);
        assert_eq!(detail.consecutive_failures, 2);
        assert_eq!(detail.cooldown_remaining_secs, None, "降级不显示冷却");
    }

    #[test]
    fn auth_failures_warn_then_degrade() {
        let m = manager(&[0, 0]);
        m.report_failure(1, test_error());
        assert_eq!(health(&m, 1), HealthStatus::Warning);
        m.report_failure(1, test_error());
        assert_eq!(health(&m, 1), HealthStatus::Degraded);
    }

    #[tokio::test]
    async fn degraded_account_is_last_within_its_priority() {
        // #1、#2 同为最高优先级，#3 次一级
        let m = manager(&[0, 0, 1]);
        m.report_throttled(1);
        m.report_throttled(1);
        assert_eq!(health(&m, 1), HealthStatus::Degraded);
        for _ in 0..4 {
            assert_eq!(m.acquire_context(None).await.unwrap().id, 2);
        }

        // 同优先级都降级：仍先于下一优先级
        m.report_throttled(2);
        m.report_throttled(2);
        for _ in 0..4 {
            assert_ne!(m.acquire_context(None).await.unwrap().id, 3);
        }
    }

    /// 账号连续限流 3 次进入冷却
    fn cool_down(m: &MultiTokenManager, ids: &[u64]) {
        for &id in ids {
            for _ in 0..3 {
                m.report_throttled(id);
            }
            assert_eq!(health(m, id), HealthStatus::Unhealthy);
        }
    }

    /// 让账号的试探时间已到（冷却仍在）
    fn make_probe_due(m: &MultiTokenManager, id: u64) {
        let mut entries = m.entries.lock();
        let entry = entries.iter_mut().find(|e| e.id == id).unwrap();
        entry.probe.reset();
        entry.probe.start(Instant::now() - secs(31));
    }

    /// 账号 `ago` 之前连续限流 3 次进入冷却；`ago` 超过 2 分钟则限流冷却已结束，账号待验证
    fn cooled_down_ago(m: &MultiTokenManager, id: u64, ago: Duration) {
        let mut entries = m.entries.lock();
        let entry = entries.iter_mut().find(|e| e.id == id).unwrap();
        let at = Instant::now() - ago;
        for _ in 0..3 {
            entry
                .upstream_failures
                .record(UpstreamFailure::Throttled, at);
        }
        entry.probe.start(at);
        entry.awaiting_verification = true;
    }

    async fn cooling_wait(m: &MultiTokenManager) -> Duration {
        let Some(err) = m.acquire_context(None).await.err() else {
            panic!("应无号可用，实际选到了账号");
        };
        err.downcast_ref::<AccountsCoolingDown>()
            .unwrap_or_else(|| panic!("应为全部冷却或待验证，实际: {err:#}"))
            .retry_after()
    }

    #[tokio::test]
    async fn all_cooling_accounts_wait_for_probe_interval() {
        let m = manager(&[0, 0]);
        cool_down(&m, &[1, 2]);
        let wait = cooling_wait(&m).await;
        assert!(
            wait > secs(29) && wait <= secs(30),
            "试探冷却从进入冷却起算 30 秒，实际 {wait:?}"
        );
    }

    #[test]
    fn panel_shows_probe_wait_from_the_start_of_cooling() {
        let m = manager(&[0]);
        assert_eq!(m.snapshot().entries[0].health_detail.probe_wait_secs, None);

        cool_down(&m, &[1]);

        let detail = &m.snapshot().entries[0].health_detail;
        assert!(matches!(detail.probe_wait_secs, Some(29..=30)));
        assert!(
            matches!(detail.cooldown_remaining_secs, Some(119..=120)),
            "冷却取限流冷却（2 分钟）与试探等待（30 秒）的较大者: {:?}",
            detail.cooldown_remaining_secs
        );
    }

    #[tokio::test]
    async fn due_probe_releases_one_request_per_account() {
        let m = manager(&[0, 0]);
        cool_down(&m, &[1, 2]);
        make_probe_due(&m, 1);
        make_probe_due(&m, 2);

        let mut picked = vec![
            m.acquire_context(None).await.unwrap().id,
            m.acquire_context(None).await.unwrap().id,
        ];
        picked.sort();
        assert_eq!(picked, [1, 2], "每个账号各放一个试探");
        let wait = cooling_wait(&m).await;
        assert!(
            wait > secs(59) && wait <= secs(60),
            "放出即算一次失败，下一次间隔翻倍到 60 秒: {wait:?}"
        );
    }

    #[tokio::test]
    async fn failed_probe_extends_cooldown_to_the_next_probe_time() {
        let m = manager(&[0]);
        // 限流冷却还剩约 20 秒，试探时间早已到
        cooled_down_ago(&m, 1, secs(100));
        assert_eq!(health(&m, 1), HealthStatus::Unhealthy);
        assert_eq!(m.acquire_context(None).await.unwrap().id, 1, "放出试探");

        m.report_throttled(1);

        let detail = &m.snapshot().entries[0].health_detail;
        assert_eq!(detail.consecutive_failures, 3, "试探失败不累加限流冷却");
        assert!(matches!(detail.probe_wait_secs, Some(59..=60)));
        assert!(
            matches!(detail.cooldown_remaining_secs, Some(59..=60)),
            "冷却取限流冷却（20 秒）与试探等待（60 秒）的较大者: {:?}",
            detail.cooldown_remaining_secs
        );
        let now = Instant::now();
        assert_eq!(
            health_at(&m, 1, now + secs(40)),
            HealthStatus::Unhealthy,
            "限流冷却结束后仍冷却到下一次试探时间"
        );
        assert_eq!(
            health_at(&m, 1, now + secs(61)),
            HealthStatus::PendingVerification
        );
    }

    #[tokio::test]
    async fn completed_probe_returns_to_health() {
        let m = manager(&[0]);
        cool_down(&m, &[1]);
        make_probe_due(&m, 1);
        assert_eq!(m.acquire_context(None).await.unwrap().id, 1);

        m.report_response_completed(1, None);

        assert_eq!(health(&m, 1), HealthStatus::Warning);
        assert_eq!(m.snapshot().entries[0].health_detail.probe_wait_secs, None);
        assert_eq!(m.acquire_context(None).await.unwrap().id, 1);
    }

    #[test]
    fn failures_flooding_in_during_cooldown_do_not_escalate_it() {
        let m = manager(&[0]);
        for _ in 0..3 {
            m.report_throttled(1);
        }
        for _ in 0..10 {
            m.report_throttled(1);
        }

        let detail = &m.snapshot().entries[0].health_detail;
        assert_eq!(detail.consecutive_failures, 3);
        assert!(
            matches!(detail.cooldown_remaining_secs, Some(119..=120)),
            "冷却仍是 2 分钟: {:?}",
            detail.cooldown_remaining_secs
        );
    }

    #[test]
    fn failure_after_leaving_cooldown_escalates_it() {
        let m = manager(&[0]);
        {
            let mut entries = m.entries.lock();
            let earlier = Instant::now() - 3 * MINUTE;
            for _ in 0..3 {
                entries[0]
                    .upstream_failures
                    .record(UpstreamFailure::Throttled, earlier);
            }
        }
        assert_ne!(health(&m, 1), HealthStatus::Unhealthy, "2 分钟冷却已过");

        m.report_throttled(1);

        assert_eq!(health(&m, 1), HealthStatus::Unhealthy);
        let detail = &m.snapshot().entries[0].health_detail;
        assert_eq!(detail.consecutive_failures, 4);
        assert!(
            matches!(detail.cooldown_remaining_secs, Some(239..=240)),
            "第 4 次连续失败冷却 4 分钟: {:?}",
            detail.cooldown_remaining_secs
        );
    }

    #[test]
    fn cooling_ended_account_awaits_verification_until_a_response_completes() {
        let m = manager(&[0]);
        cooled_down_ago(&m, 1, secs(200));
        assert_eq!(health(&m, 1), HealthStatus::PendingVerification);

        m.report_response_completed(1, None);

        // 窗口内仍有 3 次限流，回到健康组里的警告
        assert_eq!(health(&m, 1), HealthStatus::Warning);
    }

    #[tokio::test]
    async fn pending_verification_admits_one_request_at_a_time() {
        let m = manager(&[0]);
        cooled_down_ago(&m, 1, secs(200));

        let first = m.acquire_context(None).await.unwrap();
        assert!(first.trial.is_some(), "占用试验名额");
        assert_eq!(cooling_wait(&m).await, secs(1), "名额被占，稍后重选");

        drop(first);
        let second = m.acquire_context(None).await.unwrap();
        assert_eq!(second.id, 1, "名额随上下文释放");
    }

    #[tokio::test]
    async fn pending_verification_takes_part_in_normal_rotation() {
        // 不降档：待验证账号和健康账号轮流被选中，才等得到验证
        let m = manager(&[0, 0]);
        cooled_down_ago(&m, 1, secs(200));
        let mut picked = Vec::new();
        for _ in 0..4 {
            picked.push(m.acquire_context(None).await.unwrap().id);
        }
        assert!(
            picked.contains(&1) && picked.contains(&2),
            "待验证账号参与轮询: {picked:?}"
        );
    }

    #[tokio::test]
    async fn failed_trial_returns_to_cooling_with_a_longer_cooldown() {
        let m = manager(&[0]);
        cooled_down_ago(&m, 1, secs(200));
        let ctx = m.acquire_context(None).await.unwrap();

        // 验证失败是离开冷却后的再次失败，照常累加
        m.report_throttled(1);
        drop(ctx);

        assert_eq!(health(&m, 1), HealthStatus::Unhealthy);
        let detail = &m.snapshot().entries[0].health_detail;
        assert_eq!(detail.consecutive_failures, 4);
        assert!(
            matches!(detail.cooldown_remaining_secs, Some(239..=240)),
            "第 4 次连续失败冷却 4 分钟: {:?}",
            detail.cooldown_remaining_secs
        );
        assert!(matches!(detail.probe_wait_secs, Some(29..=30)));
    }

    #[tokio::test]
    async fn completed_response_resets_cooldown_and_probe_accumulation() {
        let m = manager(&[0]);
        // 第一轮 2 分钟冷却已过，验证失败：第 4 次连续失败，冷却 4 分钟
        cooled_down_ago(&m, 1, secs(200));
        m.report_throttled(1);
        {
            let mut entries = m.entries.lock();
            for _ in 0..4 {
                entries[0].probe.claim(Instant::now());
            }
        }
        let escalated = cooling_wait(&m).await;
        assert!(escalated > secs(290), "试探间隔已封顶: {escalated:?}");
        let detail = &m.snapshot().entries[0].health_detail;
        assert!(
            matches!(detail.cooldown_remaining_secs, Some(299..=300)),
            "冷却取限流冷却（4 分钟）与试探等待（5 分钟）的较大者: {:?}",
            detail.cooldown_remaining_secs
        );

        m.report_response_completed(1, None);

        assert_ne!(health(&m, 1), HealthStatus::Unhealthy);
        assert_eq!(m.snapshot().entries[0].health_detail.probe_wait_secs, None);
        for _ in 0..3 {
            m.report_throttled(1);
        }
        let detail = &m.snapshot().entries[0].health_detail;
        assert!(
            matches!(detail.cooldown_remaining_secs, Some(119..=120)),
            "冷却从 2 分钟重新算: {:?}",
            detail.cooldown_remaining_secs
        );
        let wait = cooling_wait(&m).await;
        assert!(
            wait > secs(29) && wait <= secs(30),
            "试探间隔从 30 秒重新算: {wait:?}"
        );
    }

    #[tokio::test]
    async fn unreachable_accounts_are_probed_too() {
        let m = manager(&[0]);
        for _ in 0..3 {
            m.report_unreachable(1, test_error());
        }
        let wait = cooling_wait(&m).await;
        assert!(wait > secs(29) && wait <= secs(30), "{wait:?}");
    }

    #[tokio::test]
    async fn healthy_account_is_used_while_others_wait() {
        let m = manager(&[0, 0, 1]);
        cool_down(&m, &[1, 2]);
        for _ in 0..4 {
            assert_eq!(m.acquire_context(None).await.unwrap().id, 3);
        }
    }

    #[tokio::test]
    async fn balanced_mode_skips_degraded_unless_all_degraded() {
        let m = manager(&[0, 0]);
        m.set_load_balancing_mode("balanced".to_string()).unwrap();
        m.report_throttled(1);
        m.report_throttled(1);
        for _ in 0..4 {
            assert_eq!(m.acquire_context(None).await.unwrap().id, 2);
        }

        m.report_throttled(2);
        m.report_throttled(2);
        let mut picked = Vec::new();
        for _ in 0..4 {
            picked.push(m.acquire_context(None).await.unwrap().id);
        }
        assert!(
            picked.contains(&1) && picked.contains(&2),
            "全部降级时照常轮询: {picked:?}"
        );
    }

    #[test]
    fn reset_state_clears_cooling_and_degradation() {
        let m = manager(&[0, 0]);
        cool_down(&m, &[1]);
        m.report_throttled(2);
        m.report_throttled(2);
        assert_eq!(health(&m, 2), HealthStatus::Degraded);

        m.reset_state(1).unwrap();
        m.reset_state(2).unwrap();

        assert_eq!(health(&m, 1), HealthStatus::Healthy);
        assert_eq!(health(&m, 2), HealthStatus::Healthy);
        assert_eq!(m.snapshot().entries[0].health_detail.probe_wait_secs, None);
    }

    #[test]
    fn reset_state_clears_pending_verification() {
        let m = manager(&[0]);
        cooled_down_ago(&m, 1, secs(200));
        assert_eq!(health(&m, 1), HealthStatus::PendingVerification);

        m.reset_state(1).unwrap();

        assert_eq!(health(&m, 1), HealthStatus::Healthy);
    }

    #[test]
    fn reset_state_keeps_disabled_accounts_disabled() {
        let m = manager(&[0]);
        {
            let mut entries = m.entries.lock();
            entries[0].disabled = true;
            entries[0].disabled_reason = Some(DisabledReason::Manual);
        }

        m.reset_state(1).unwrap();

        let entries = m.entries.lock();
        assert!(entries[0].disabled);
        assert_eq!(entries[0].disabled_reason, Some(DisabledReason::Manual));
    }

    #[test]
    fn reset_state_rejects_unknown_account() {
        let m = manager(&[0]);
        let error = m.reset_state(99).unwrap_err();
        assert!(error.to_string().contains("不存在"), "{error:#}");
    }

    #[tokio::test]
    async fn reset_and_enable_also_clears_throttle_cooling() {
        let m = manager(&[0]);
        // 已知订阅等级时启用不需要联网查询
        m.entries.lock()[0].credentials.subscription_title = Some("KIRO FREE".to_string());
        cool_down(&m, &[1]);

        m.reset_and_enable(1).await.unwrap();

        assert_eq!(health(&m, 1), HealthStatus::Healthy);
    }

    #[tokio::test]
    async fn ignore_health_selects_cooling_accounts_without_waiting() {
        let m = manager(&[0, 0]);
        cool_down(&m, &[1, 2]);
        cooling_wait(&m).await;

        m.set_ignore_health(true);
        let mut picked = Vec::new();
        for _ in 0..4 {
            picked.push(m.acquire_context(None).await.unwrap().id);
        }
        assert!(
            picked.contains(&1) && picked.contains(&2),
            "冷却中的账号照常轮询: {picked:?}"
        );
        assert_eq!(health(&m, 1), HealthStatus::Unhealthy, "面板仍显示真实状态");
        assert!(
            matches!(
                m.snapshot().entries[0].health_detail.probe_wait_secs,
                Some(29..=30)
            ),
            "不占试探名额，试探间隔不翻倍"
        );

        m.set_ignore_health(false);
        let wait = cooling_wait(&m).await;
        assert!(
            wait > secs(29) && wait <= secs(30),
            "关闭后恢复遵守冷却: {wait:?}"
        );
    }

    #[tokio::test]
    async fn ignore_health_does_not_limit_pending_verification_to_one_request() {
        let m = manager(&[0]);
        cooled_down_ago(&m, 1, secs(200));
        m.set_ignore_health(true);

        let first = m.acquire_context(None).await.unwrap();
        let second = m.acquire_context(None).await.unwrap();

        assert!(first.trial.is_none() && second.trial.is_none());
    }

    #[tokio::test]
    async fn ignore_health_does_not_rank_degraded_last() {
        let m = manager(&[0, 0]);
        m.report_throttled(1);
        m.report_throttled(1);
        assert_eq!(health(&m, 1), HealthStatus::Degraded);

        m.set_ignore_health(true);
        let mut picked = Vec::new();
        for _ in 0..4 {
            picked.push(m.acquire_context(None).await.unwrap().id);
        }

        assert!(picked.contains(&1), "降级账号不再排末位: {picked:?}");
    }

    #[tokio::test]
    async fn ignore_health_still_skips_disabled_accounts() {
        let m = manager(&[0, 0]);
        cool_down(&m, &[1]);
        {
            let mut entries = m.entries.lock();
            entries[1].disabled = true;
            entries[1].disabled_reason = Some(DisabledReason::Manual);
        }
        m.set_ignore_health(true);

        for _ in 0..4 {
            assert_eq!(m.acquire_context(None).await.unwrap().id, 1);
        }
    }

    #[test]
    fn ignore_health_is_off_by_default() {
        let m = manager(&[0]);
        assert!(!m.ignore_health());
        m.set_ignore_health(true);
        assert!(m.ignore_health());
    }
}
