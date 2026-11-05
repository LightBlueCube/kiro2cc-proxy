//! 候选账号全部在冷却时的试探间隔
//!
//! 正常选号排除冷却中（不健康）的账号；候选账号全都冷却时，只放行试探时间已到的账号，
//! 每个账号一次只放出一个试探请求。试探失败间隔翻倍，一次正常完成清零：
//!
//! ```text
//! 冷却中又失败 ─15s─▶ 试探 ─失败─30s─▶ 试探 ─失败─60s─▶ … 封顶 5 分钟
//!                       └─ 正常完成 ─▶ 清零
//! ```
//!
//! 冷却中第一次失败前不设间隔（与正常选号一样放行），仍能完成大部分请求的账号不受影响。
//! 仅存内存，重启后清空。

use std::time::{Duration, Instant};

const BASE_INTERVAL: Duration = Duration::from_secs(15);
const MAX_INTERVAL: Duration = Duration::from_secs(5 * 60);
/// 翻倍次数上限：BASE × 2^5 已超过 MAX，再多只会溢出
const MAX_DOUBLINGS: u32 = 5;

#[derive(Debug, Default)]
pub(crate) struct ProbeBackoff {
    /// 冷却中连续失败的试探次数
    failures: u32,
    /// 下一次可以放出试探的时刻
    next_at: Option<Instant>,
    /// 已放出一个试探、结果未回：它的失败计入，等待期内其它请求的失败不计
    probing: bool,
}

impl ProbeBackoff {
    /// 第 `failures` 次试探失败后的等待时长（`failures` ≥ 1）
    fn interval(failures: u32) -> Duration {
        let doublings = failures.saturating_sub(1).min(MAX_DOUBLINGS);
        (BASE_INTERVAL * 2u32.pow(doublings)).min(MAX_INTERVAL)
    }

    /// 距下一次可以试探的剩余时长；现在就可以放行时为 None
    pub(crate) fn wait_remaining(&self, now: Instant) -> Option<Duration> {
        self.next_at?
            .checked_duration_since(now)
            .filter(|d| !d.is_zero())
    }

    /// 已开始按间隔试探（冷却中至少又失败过一次）
    pub(crate) fn is_active(&self) -> bool {
        self.failures > 0
    }

    /// 放出一个试探：结果回来前占住名额，按再失败一次的间隔推迟下一次试探
    pub(crate) fn claim(&mut self, now: Instant) {
        if !self.is_active() {
            return;
        }
        self.probing = true;
        self.next_at = Some(now + Self::interval(self.failures + 1));
    }

    /// 冷却中的账号又失败一次
    ///
    /// 试探失败或已过等待期时间隔翻倍；等待期内其它请求的失败（冷却前发出、晚到的）不计。
    pub(crate) fn record_failure(&mut self, now: Instant) {
        if !self.probing && self.wait_remaining(now).is_some() {
            return;
        }
        self.failures = self.failures.saturating_add(1);
        self.probing = false;
        self.next_at = Some(now + Self::interval(self.failures));
    }

    /// 正常完成一次响应或手动恢复
    pub(crate) fn reset(&mut self) {
        *self = Self::default();
    }
}

/// 候选账号全部在冷却、且都没到试探时间
#[derive(Debug)]
pub struct AccountsCoolingDown {
    retry_after: Duration,
}

impl AccountsCoolingDown {
    pub(crate) fn new(retry_after: Duration) -> Self {
        Self { retry_after }
    }

    /// 最早一个账号可以试探的剩余时长
    pub fn retry_after(&self) -> Duration {
        self.retry_after
    }

    /// 告知客户端的等待秒数（向上取整，至少 1 秒）
    pub fn retry_after_secs(&self) -> u64 {
        self.retry_after.as_secs_f64().ceil().max(1.0) as u64
    }
}

impl std::fmt::Display for AccountsCoolingDown {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "所有候选账号都在冷却中，{}s 后才能再试探",
            self.retry_after_secs()
        )
    }
}

impl std::error::Error for AccountsCoolingDown {}

#[cfg(test)]
mod tests {
    use super::*;

    fn secs(n: u64) -> Duration {
        Duration::from_secs(n)
    }

    #[test]
    fn first_failure_starts_at_15s_then_doubles_up_to_5_minutes() {
        let mut p = ProbeBackoff::default();
        let now = Instant::now();
        assert!(!p.is_active());
        assert_eq!(p.wait_remaining(now), None);

        for expected in [15, 30, 60, 120, 240, 300, 300] {
            p.claim(now);
            p.record_failure(now);
            assert_eq!(p.wait_remaining(now), Some(secs(expected)));
        }
    }

    #[test]
    fn claim_holds_the_slot_until_the_probe_returns() {
        let mut p = ProbeBackoff::default();
        let t0 = Instant::now();
        p.record_failure(t0);

        let t1 = t0 + secs(15);
        assert_eq!(p.wait_remaining(t1), None, "到点可以试探");
        p.claim(t1);
        assert_eq!(
            p.wait_remaining(t1),
            Some(secs(30)),
            "试探结果回来前不放第二个"
        );

        p.record_failure(t1 + secs(2));
        assert_eq!(p.wait_remaining(t1 + secs(2)), Some(secs(30)));
    }

    #[test]
    fn late_failures_during_wait_are_ignored() {
        let mut p = ProbeBackoff::default();
        let t0 = Instant::now();
        p.record_failure(t0);
        p.record_failure(t0 + secs(1));
        p.record_failure(t0 + secs(2));
        assert_eq!(
            p.wait_remaining(t0),
            Some(secs(15)),
            "等待期内晚到的失败不翻倍"
        );
    }

    #[test]
    fn claim_before_any_failure_does_not_gate() {
        let mut p = ProbeBackoff::default();
        let now = Instant::now();
        p.claim(now);
        assert_eq!(p.wait_remaining(now), None);
    }

    #[test]
    fn reset_clears_everything() {
        let mut p = ProbeBackoff::default();
        let now = Instant::now();
        p.record_failure(now);
        p.reset();
        assert!(!p.is_active());
        assert_eq!(p.wait_remaining(now), None);
    }

    #[test]
    fn retry_after_secs_rounds_up() {
        assert_eq!(
            AccountsCoolingDown::new(Duration::from_millis(14_200)).retry_after_secs(),
            15
        );
        assert_eq!(
            AccountsCoolingDown::new(Duration::from_millis(10)).retry_after_secs(),
            1
        );
    }
}
