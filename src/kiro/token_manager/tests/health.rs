// Copyright (c) 2026 Harllan He. Licensed under MIT.
// 健康度判定（近期上游失败）与降级账号的选号顺位
#[cfg(test)]
mod tests {
    use super::super::super::entry::HealthStatus;
    use super::super::super::types::MultiTokenManager;
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
        // 冷却 2 分钟后放回选号（仍是降级），再失败一次冷却翻倍到 4 分钟
        assert_eq!(
            health_at(&m, 1, now + 2 * MINUTE + Duration::from_secs(1)),
            HealthStatus::Degraded
        );
        m.report_throttled(1);
        let now = Instant::now();
        assert_eq!(health_at(&m, 1, now + 3 * MINUTE), HealthStatus::Unhealthy);
        assert_eq!(
            health_at(&m, 1, now + 4 * MINUTE + Duration::from_secs(1)),
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
