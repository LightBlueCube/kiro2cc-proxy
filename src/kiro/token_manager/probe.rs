//! 账号冷却期间的试探间隔
//!
//! 账号进入冷却时开始计时（起步 30 秒）。候选账号全部冷却时，只放行试探时间已到的账号，
//! 每个账号一次只放出一个试探请求。放出即算一次失败：下一次间隔翻倍，账号在下次试探时间之前
//! 一直算冷却（冷却取限流冷却与试探等待的较大者）。一次完整的正常响应清零：
//!
//! ```text
//! 进入冷却 ─30s─▶ 试探 ─60s─▶ 试探 ─120s─▶ 试探 ─240s─▶ 试探 ─300s─▶ … 封顶 5 分钟
//!                  └─ 正常完成 ─▶ 清零，账号回到健康
//! ```
//!
//! 次数只在放出试探时增加，冷却期内灌进来的其它失败不影响间隔。清零只靠正常完成或手动恢复，
//! 账号冷却到期、经待验证再次进入冷却时沿用累计的次数。仅存内存，重启后清空。

use std::time::{Duration, Instant};

const BASE_INTERVAL: Duration = Duration::from_secs(30);
const MAX_INTERVAL: Duration = Duration::from_secs(5 * 60);
/// 翻倍次数上限：BASE × 2^4 已超过 MAX，再多只会溢出
const MAX_DOUBLINGS: u32 = 4;

#[derive(Debug, Default)]
pub(crate) struct ProbeBackoff {
    /// 已放出的试探次数（没有完整完成的都算失败），决定下一次间隔
    probes: u32,
    /// 下一次可以放出试探的时刻；账号进入过冷却后才有值
    next_at: Option<Instant>,
}

impl ProbeBackoff {
    /// 已放出 `probes` 次试探后的等待时长
    fn interval(probes: u32) -> Duration {
        (BASE_INTERVAL * 2u32.pow(probes.min(MAX_DOUBLINGS))).min(MAX_INTERVAL)
    }

    /// 距下一次可以试探的剩余时长；现在就可以放行时为 None
    pub(crate) fn wait_remaining(&self, now: Instant) -> Option<Duration> {
        self.next_at?
            .checked_duration_since(now)
            .filter(|d| !d.is_zero())
    }

    /// 账号进入冷却：等一个间隔后才放出第一个试探
    pub(crate) fn start(&mut self, now: Instant) {
        self.next_at = Some(now + Self::interval(self.probes));
    }

    /// 放出一个试探：算一次失败，下一次间隔翻倍
    ///
    /// 账号在新的试探时间之前一直算冷却，所以不必等试探失败的上报。
    pub(crate) fn claim(&mut self, now: Instant) {
        self.probes = self.probes.saturating_add(1);
        self.next_at = Some(now + Self::interval(self.probes));
    }

    /// 正常完成一次响应或手动恢复
    pub(crate) fn reset(&mut self) {
        *self = Self::default();
    }
}

/// 没有账号可以立即使用：全部在冷却且都没到试探时间，或待验证账号的试验名额被占用
#[derive(Debug)]
pub struct AccountsCoolingDown {
    retry_after: Duration,
}

impl AccountsCoolingDown {
    pub(crate) fn new(retry_after: Duration) -> Self {
        Self { retry_after }
    }

    /// 最早一个账号可以使用的剩余时长
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
            "所有候选账号都在冷却中或待验证，{}s 后才能再使用",
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
    fn first_probe_waits_30s_then_each_claim_doubles_up_to_5_minutes() {
        let mut p = ProbeBackoff::default();
        let now = Instant::now();
        assert_eq!(p.wait_remaining(now), None, "没进入过冷却不设间隔");

        p.start(now);
        assert_eq!(p.wait_remaining(now), Some(secs(30)));

        for expected in [60, 120, 240, 300, 300] {
            p.claim(now);
            assert_eq!(p.wait_remaining(now), Some(secs(expected)));
        }
    }

    #[test]
    fn probe_is_due_once_the_wait_has_passed() {
        let mut p = ProbeBackoff::default();
        let t0 = Instant::now();
        p.start(t0);
        assert_eq!(p.wait_remaining(t0 + secs(29)), Some(secs(1)));
        assert_eq!(p.wait_remaining(t0 + secs(30)), None);
    }

    #[test]
    fn claim_holds_the_slot_until_the_next_probe_time() {
        let mut p = ProbeBackoff::default();
        let t0 = Instant::now();
        p.start(t0);

        let t1 = t0 + secs(30);
        p.claim(t1);
        assert_eq!(p.wait_remaining(t1), Some(secs(60)), "放出后不放第二个");
    }

    #[test]
    fn next_cooling_round_continues_from_accumulated_probes() {
        let mut p = ProbeBackoff::default();
        let now = Instant::now();
        p.start(now);
        p.claim(now);
        p.claim(now);

        p.start(now);
        assert_eq!(p.wait_remaining(now), Some(secs(120)));
    }

    #[test]
    fn reset_clears_everything() {
        let mut p = ProbeBackoff::default();
        let now = Instant::now();
        p.start(now);
        p.claim(now);
        p.reset();
        assert_eq!(p.wait_remaining(now), None);

        p.start(now);
        assert_eq!(p.wait_remaining(now), Some(secs(30)));
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
