//! 可能自行恢复的连续失败的冷却，两处共用：
//! - 账号连不上上游（请求发送失败：代理错误、连接失败、超时）：冷却整个账号
//! - 账号的某个模型连续被上游拒答（含「因拒答过多被临时限制」）：只冷却该模型
//!
//! 不禁用账号：连续 3 次进入冷却，冷却时长逐轮翻倍（10 → 20 → 40 → 80 → 120 分钟封顶），
//! 到期自动重新参与选号。一次完整的正常响应清零全部状态。仅存内存，重启后清空。

use std::time::{Duration, Instant};

/// 连续失败达到此数进入冷却
const COOLDOWN_STREAK: u32 = 3;
const BASE_COOLDOWN: Duration = Duration::from_secs(10 * 60);
const MAX_COOLDOWN: Duration = Duration::from_secs(2 * 60 * 60);
/// 翻倍次数上限：BASE × 2^4 已超过 MAX，再多只会溢出
const MAX_DOUBLINGS: u32 = 4;

#[derive(Debug, Default)]
pub(crate) struct FailureCooldown {
    /// 本轮连续失败次数（进入冷却后清零，冷却到期重新累计）
    streak: u32,
    /// 已进入过的冷却轮数，决定下一轮冷却时长
    rounds: u32,
    until: Option<Instant>,
}

impl FailureCooldown {
    /// 记一次失败；本次触发冷却时返回冷却时长
    ///
    /// 冷却期内的失败不计数：所有账号都不可用时，冷却中的账号仍可能被选中。
    pub(crate) fn record(&mut self, now: Instant) -> Option<Duration> {
        if self.cooldown_remaining(now).is_some() {
            return None;
        }
        self.streak += 1;
        if self.streak < COOLDOWN_STREAK {
            return None;
        }

        let cooldown = (BASE_COOLDOWN * 2u32.pow(self.rounds.min(MAX_DOUBLINGS))).min(MAX_COOLDOWN);
        self.streak = 0;
        self.rounds += 1;
        self.until = Some(now + cooldown);
        Some(cooldown)
    }

    /// 正常完成一次响应或手动恢复
    pub(crate) fn reset(&mut self) {
        *self = Self::default();
    }

    pub(crate) fn streak(&self) -> u32 {
        self.streak
    }

    pub(crate) fn cooldown_remaining(&self, now: Instant) -> Option<Duration> {
        self.until?
            .checked_duration_since(now)
            .filter(|d| !d.is_zero())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn minutes(n: u64) -> Duration {
        Duration::from_secs(n * 60)
    }

    /// 连续失败 3 次进入一轮冷却，返回本轮时长
    fn fail_round(u: &mut FailureCooldown, now: Instant) -> Option<Duration> {
        assert_eq!(u.record(now), None);
        assert_eq!(u.record(now), None);
        u.record(now)
    }

    #[test]
    fn cooldown_doubles_per_round_and_caps_at_two_hours() {
        let mut u = FailureCooldown::default();
        let mut now = Instant::now();
        for expected in [10, 20, 40, 80, 120, 120] {
            assert_eq!(fail_round(&mut u, now), Some(minutes(expected)));
            assert_eq!(u.cooldown_remaining(now), Some(minutes(expected)));
            now += minutes(expected);
            assert_eq!(u.cooldown_remaining(now), None, "到期自动解除");
        }
    }

    #[test]
    fn failures_during_cooldown_are_ignored() {
        let mut u = FailureCooldown::default();
        let now = Instant::now();
        fail_round(&mut u, now);
        for _ in 0..5 {
            assert_eq!(u.record(now + minutes(1)), None);
        }
        assert_eq!(u.streak(), 0);
        assert_eq!(u.cooldown_remaining(now + minutes(1)), Some(minutes(9)));
    }

    #[test]
    fn reset_restarts_from_base_cooldown() {
        let mut u = FailureCooldown::default();
        let now = Instant::now();
        fail_round(&mut u, now);
        u.reset();
        assert_eq!(u.cooldown_remaining(now), None);
        assert_eq!(fail_round(&mut u, now), Some(minutes(10)));
    }
}
