//! 账号近期的上游失败（限流、异常结束）：最近 10 分钟的分钟级计数与连续失败次数，健康度据此判定。
//!
//! 只记上游失败，不看成功次数：长期成功的历史不应稀释眼前的限流。连续失败由一次完整的
//! 正常响应中断（[`UpstreamFailures::end_streak`]）。冷却期内的失败不计，离开冷却后再失败才升级
//! 冷却。仅存内存，重启后清空。

use std::time::{Duration, Instant};

/// 统计窗口长度（分钟）
const WINDOW_MINUTES: u64 = 10;
const SECS_PER_MINUTE: u64 = 60;

/// 连续失败达到此数（且在冷却期内）判为不健康
const UNHEALTHY_CONSECUTIVE: u32 = 3;
/// 窗口内失败达到此数（且在冷却期内）判为不健康
const UNHEALTHY_RECENT: u32 = 20;
/// 连续失败达到此数判为降级
pub(crate) const DEGRADED_CONSECUTIVE: u32 = 2;
/// 窗口内失败达到此数判为降级
pub(crate) const DEGRADED_RECENT: u32 = 10;
/// 窗口内失败达到此数判为警告
pub(crate) const WARNING_RECENT: u32 = 2;

/// 不健康冷却期的起始时长与上限：连续失败每多 1 次翻倍
const BASE_COOLDOWN: Duration = Duration::from_secs(2 * SECS_PER_MINUTE);
const MAX_COOLDOWN: Duration = Duration::from_secs(WINDOW_MINUTES * SECS_PER_MINUTE);

/// 计入健康度的上游失败
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum UpstreamFailure {
    /// HTTP 429 或流内限流帧
    Throttled,
    /// 200 响应异常结束：空响应（上下文过大导致的不算）、流内错误帧或截断、thinking 后无正文结束
    Abnormal,
}

/// 最近 10 分钟按种类计的失败次数
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub(crate) struct RecentFailures {
    pub(crate) throttled: u32,
    pub(crate) abnormal: u32,
}

impl RecentFailures {
    pub(crate) fn total(&self) -> u32 {
        self.throttled + self.abnormal
    }
}

#[derive(Debug, Clone, Copy, Default)]
struct MinuteBucket {
    /// 自 `origin` 起的分钟序号
    minute: u64,
    failures: RecentFailures,
}

pub(crate) struct UpstreamFailures {
    /// 分钟序号的起点
    origin: Instant,
    /// 环形分钟桶，下标 = 分钟序号 % WINDOW_MINUTES
    buckets: [MinuteBucket; WINDOW_MINUTES as usize],
    consecutive: u32,
    last_at: Option<Instant>,
}

impl UpstreamFailures {
    pub(crate) fn new(origin: Instant) -> Self {
        Self {
            origin,
            buckets: [MinuteBucket::default(); WINDOW_MINUTES as usize],
            consecutive: 0,
            last_at: None,
        }
    }

    /// 记一次失败
    ///
    /// 冷却期内的失败不计：所有账号都在冷却时冷却中的账号仍可能被选中，冷却前发出的在途请求
    /// 和同一请求的后续重试也会陆续失败。只有离开冷却后的再次失败才升级冷却。
    pub(crate) fn record(&mut self, kind: UpstreamFailure, now: Instant) {
        if self.is_cooling(now) {
            return;
        }

        let minute = self.minute_of(now);
        let bucket = &mut self.buckets[(minute % WINDOW_MINUTES) as usize];
        if bucket.minute != minute {
            *bucket = MinuteBucket {
                minute,
                failures: RecentFailures::default(),
            };
        }
        match kind {
            UpstreamFailure::Throttled => bucket.failures.throttled += 1,
            UpstreamFailure::Abnormal => bucket.failures.abnormal += 1,
        }
        self.consecutive = self.consecutive.saturating_add(1);
        self.last_at = Some(now);
    }

    /// 一次完整的正常响应：连续失败中断（窗口计数保留，随时间过期）
    pub(crate) fn end_streak(&mut self) {
        self.consecutive = 0;
    }

    pub(crate) fn recent(&self, now: Instant) -> RecentFailures {
        let now_minute = self.minute_of(now);
        self.buckets
            .iter()
            // 并发下记录时刻可能略晚于传入的 now，差值按 0 计
            .filter(|b| now_minute.saturating_sub(b.minute) < WINDOW_MINUTES)
            .fold(RecentFailures::default(), |sum, b| RecentFailures {
                throttled: sum.throttled + b.failures.throttled,
                abnormal: sum.abnormal + b.failures.abnormal,
            })
    }

    /// 连续失败次数；最后一次失败已滑出窗口时按 0 计
    ///
    /// 降级账号很少被选中，未必等得到一次正常完成来清零；不让过期的连续计数一直压着它。
    pub(crate) fn consecutive(&self, now: Instant) -> u32 {
        let window = Duration::from_secs(WINDOW_MINUTES * SECS_PER_MINUTE);
        match self.last_at {
            Some(last) if now.saturating_duration_since(last) < window => self.consecutive,
            _ => 0,
        }
    }

    /// 不健康冷却期剩余时长；冷却期从最后一次失败起算，已过或从未失败时为 None
    ///
    /// 冷却时长随连续失败翻倍：3 次及以下 2 分钟，4 次 4 分钟，5 次 8 分钟，再多封顶 10 分钟。
    /// 冷却期满后账号重新参与选号，相当于试探一次。
    pub(crate) fn cooldown_remaining(&self, now: Instant) -> Option<Duration> {
        let doublings = self
            .consecutive
            .saturating_sub(UNHEALTHY_CONSECUTIVE)
            .min(3);
        let cooldown = (BASE_COOLDOWN * 2u32.pow(doublings)).min(MAX_COOLDOWN);
        let elapsed = now.saturating_duration_since(self.last_at?);
        cooldown.checked_sub(elapsed).filter(|d| !d.is_zero())
    }

    /// 处于不健康冷却期内：连续失败或窗口内失败达到阈值，且冷却期未满
    pub(crate) fn is_cooling(&self, now: Instant) -> bool {
        let failing_hard = self.consecutive(now) >= UNHEALTHY_CONSECUTIVE
            || self.recent(now).total() >= UNHEALTHY_RECENT;
        failing_hard && self.cooldown_remaining(now).is_some()
    }

    fn minute_of(&self, now: Instant) -> u64 {
        now.saturating_duration_since(self.origin).as_secs() / SECS_PER_MINUTE
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn minutes(n: u64) -> Duration {
        Duration::from_secs(n * SECS_PER_MINUTE)
    }

    #[test]
    fn recent_counts_by_kind_and_expires_after_window() {
        let t0 = Instant::now();
        let mut f = UpstreamFailures::new(t0);
        f.record(UpstreamFailure::Throttled, t0);
        f.record(UpstreamFailure::Throttled, t0 + Duration::from_secs(30));
        f.record(UpstreamFailure::Abnormal, t0 + minutes(3));

        let at_5 = f.recent(t0 + minutes(5));
        assert_eq!(
            at_5,
            RecentFailures {
                throttled: 2,
                abnormal: 1
            }
        );
        // 第 0 分钟的桶在第 10 分钟滑出窗口，第 3 分钟的仍在
        assert_eq!(f.recent(t0 + minutes(10)).total(), 1);
        assert_eq!(f.recent(t0 + minutes(13)).total(), 0);
    }

    #[test]
    fn reused_bucket_drops_old_minute() {
        let t0 = Instant::now();
        let mut f = UpstreamFailures::new(t0);
        f.record(UpstreamFailure::Throttled, t0);
        // 第 10 分钟与第 0 分钟同一个桶：先清掉旧计数再累加
        f.record(UpstreamFailure::Abnormal, t0 + minutes(10));
        assert_eq!(
            f.recent(t0 + minutes(10)),
            RecentFailures {
                throttled: 0,
                abnormal: 1
            }
        );
    }

    #[test]
    fn streak_ends_on_completion_but_window_keeps_counts() {
        let t0 = Instant::now();
        let mut f = UpstreamFailures::new(t0);
        f.record(UpstreamFailure::Throttled, t0);
        f.record(UpstreamFailure::Throttled, t0);
        assert_eq!(f.consecutive(t0), 2);

        f.end_streak();
        assert_eq!(f.consecutive(t0), 0);
        assert_eq!(f.recent(t0).total(), 2);
    }

    #[test]
    fn streak_expires_with_window() {
        let t0 = Instant::now();
        let mut f = UpstreamFailures::new(t0);
        f.record(UpstreamFailure::Abnormal, t0);
        f.record(UpstreamFailure::Abnormal, t0);
        assert_eq!(f.consecutive(t0 + minutes(9)), 2);
        assert_eq!(f.consecutive(t0 + minutes(10)), 0);
    }

    #[test]
    fn failures_during_cooldown_are_not_accumulated() {
        let t0 = Instant::now();
        let mut f = UpstreamFailures::new(t0);
        for _ in 0..3 {
            f.record(UpstreamFailure::Throttled, t0);
        }

        let during = t0 + minutes(1);
        for _ in 0..5 {
            f.record(UpstreamFailure::Throttled, during);
        }
        assert_eq!(f.consecutive(during), 3);
        assert_eq!(f.recent(during).total(), 3);
        assert_eq!(
            f.cooldown_remaining(during),
            Some(minutes(1)),
            "冷却中的失败不延长冷却"
        );
    }

    #[test]
    fn cooldown_escalates_only_by_failures_after_leaving_it() {
        let t0 = Instant::now();
        let mut f = UpstreamFailures::new(t0);
        f.record(UpstreamFailure::Throttled, t0);
        f.record(UpstreamFailure::Throttled, t0);

        let mut now = t0;
        for (streak, cooldown_minutes) in [(3, 2), (4, 4), (5, 8), (6, 10), (7, 10)] {
            f.record(UpstreamFailure::Throttled, now);
            assert_eq!(f.consecutive(now), streak);
            assert_eq!(
                f.cooldown_remaining(now),
                Some(minutes(cooldown_minutes)),
                "连续 {streak} 次"
            );

            now += minutes(cooldown_minutes);
            assert_eq!(f.cooldown_remaining(now), None, "到期自动解除");
        }
    }
}
