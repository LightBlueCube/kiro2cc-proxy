// Copyright (c) 2026 Harllan He. Licensed under MIT.
// 健康度判定（近期上游失败）与降级账号的选号顺位
#[cfg(test)]
mod tests {
    use super::super::super::entry::HealthStatus;
    use super::super::super::probe::AccountsCoolingDown;
    use super::super::super::types::MultiTokenManager;
    use super::super::super::upstream_failures::UpstreamFailure;
    use super::super::affinity::tests::make_valid_cred;
    use super::super::multi::tests::test_error;
    use crate::model::config::Config;
    use std::time::{Duration, Instant};

    const MINUTE: Duration = Duration::from_secs(60);

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
    fn unhealthy_only_during_cooldown_then_probed() {
        let m = manager(&[0]);
        for _ in 0..3 {
            m.report_throttled(1);
        }
        let now = Instant::now();
        assert_eq!(health_at(&m, 1, now), HealthStatus::Unhealthy);
        // 冷却 2 分钟后放回选号（仍是降级），离开冷却后再失败一次冷却翻倍到 4 分钟
        let after_cooldown = now + 2 * MINUTE + Duration::from_secs(1);
        assert_eq!(health_at(&m, 1, after_cooldown), HealthStatus::Degraded);
        m.entries.lock()[0]
            .upstream_failures
            .record(UpstreamFailure::Throttled, after_cooldown);
        assert_eq!(
            health_at(&m, 1, after_cooldown + 3 * MINUTE),
            HealthStatus::Unhealthy
        );
        assert_eq!(
            health_at(&m, 1, after_cooldown + 4 * MINUTE + Duration::from_secs(1)),
            HealthStatus::Degraded
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
        // 空闲 10 分钟：窗口与连续计数都过期
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

    /// 账号连续限流进入冷却，且冷却中又失败一次（开始按间隔试探）
    fn cool_down_and_fail_once_more(m: &MultiTokenManager, ids: &[u64]) {
        for &id in ids {
            for _ in 0..3 {
                m.report_throttled(id);
            }
            assert_eq!(health(m, id), HealthStatus::Unhealthy);
            m.report_throttled(id);
        }
    }

    /// 让账号的试探间隔已经过去（冷却仍在）
    fn make_probe_due(m: &MultiTokenManager, id: u64) {
        let mut entries = m.entries.lock();
        let entry = entries.iter_mut().find(|e| e.id == id).unwrap();
        entry.probe.reset();
        entry
            .probe
            .record_failure(Instant::now() - Duration::from_secs(20));
    }

    async fn cooling_wait(m: &MultiTokenManager) -> Duration {
        let Some(err) = m.acquire_context(None).await.err() else {
            panic!("应为全部冷却，实际选到了账号");
        };
        err.downcast_ref::<AccountsCoolingDown>()
            .unwrap_or_else(|| panic!("应为全部冷却，实际: {err:#}"))
            .retry_after()
    }

    #[tokio::test]
    async fn all_cooling_accounts_wait_for_probe_interval() {
        let m = manager(&[0, 0]);
        cool_down_and_fail_once_more(&m, &[1, 2]);
        let wait = cooling_wait(&m).await;
        assert!(
            wait > Duration::from_secs(14) && wait <= Duration::from_secs(15),
            "首个试探间隔 15 秒，实际 {wait:?}"
        );
    }

    #[tokio::test]
    async fn cooling_without_extra_failure_is_still_released() {
        // 刚进入冷却、冷却中还没再失败过：与原来一样放行
        let m = manager(&[0, 0]);
        for id in [1, 2] {
            for _ in 0..3 {
                m.report_throttled(id);
            }
        }
        assert!(m.acquire_context(None).await.is_ok());
        assert_eq!(m.snapshot().entries[0].health_detail.probe_wait_secs, None);
    }

    #[tokio::test]
    async fn due_probe_releases_one_request_per_account() {
        let m = manager(&[0, 0]);
        cool_down_and_fail_once_more(&m, &[1, 2]);
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
            wait > Duration::from_secs(29),
            "试探结果回来前占住名额: {wait:?}"
        );
    }

    #[tokio::test]
    async fn failed_probe_doubles_and_completion_resets() {
        let m = manager(&[0]);
        cool_down_and_fail_once_more(&m, &[1]);
        make_probe_due(&m, 1);
        assert_eq!(m.acquire_context(None).await.unwrap().id, 1);

        m.report_throttled(1);
        let wait = cooling_wait(&m).await;
        assert!(
            wait > Duration::from_secs(29) && wait <= Duration::from_secs(30),
            "试探失败后间隔翻倍到 30 秒，实际 {wait:?}"
        );
        let shown = m.snapshot().entries[0].health_detail.probe_wait_secs;
        assert!(
            matches!(shown, Some(29..=30)),
            "面板显示试探等待: {shown:?}"
        );

        m.report_response_completed(1, None);
        assert_ne!(health(&m, 1), HealthStatus::Unhealthy);
        assert_eq!(m.acquire_context(None).await.unwrap().id, 1);
        assert_eq!(m.snapshot().entries[0].health_detail.probe_wait_secs, None);
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

    #[tokio::test]
    async fn completed_response_resets_cooldown_and_probe_accumulation() {
        let m = manager(&[0]);
        {
            let mut entries = m.entries.lock();
            // 第一轮 2 分钟冷却已过，离开冷却后再失败：第 4 次连续失败，冷却 4 分钟
            let earlier = Instant::now() - 3 * MINUTE;
            for _ in 0..3 {
                entries[0]
                    .upstream_failures
                    .record(UpstreamFailure::Throttled, earlier);
            }
            let now = Instant::now();
            entries[0]
                .upstream_failures
                .record(UpstreamFailure::Throttled, now);
            for _ in 0..7 {
                entries[0].probe.claim(now);
                entries[0].probe.record_failure(now);
            }
        }
        let escalated = cooling_wait(&m).await;
        assert!(
            escalated > Duration::from_secs(290),
            "试探间隔已封顶: {escalated:?}"
        );
        let detail = &m.snapshot().entries[0].health_detail;
        assert!(matches!(detail.cooldown_remaining_secs, Some(239..=240)));

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
        m.report_throttled(1);
        let wait = cooling_wait(&m).await;
        assert!(
            wait > Duration::from_secs(14) && wait <= Duration::from_secs(15),
            "试探间隔从 15 秒重新算: {wait:?}"
        );
    }

    #[tokio::test]
    async fn unreachable_accounts_are_probed_too() {
        let m = manager(&[0]);
        for _ in 0..3 {
            m.report_unreachable(1, test_error());
        }
        assert!(m.acquire_context(None).await.is_ok(), "冷却中还没再失败过");
        m.report_unreachable(1, test_error());
        cooling_wait(&m).await;
    }

    #[tokio::test]
    async fn healthy_account_is_used_while_others_wait() {
        let m = manager(&[0, 0, 1]);
        cool_down_and_fail_once_more(&m, &[1, 2]);
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
}
