//! 待验证账号的试验名额：同一时刻只放行一个请求
//!
//! 账号冷却结束后进入待验证，只有一次完整成功的响应能让它回到健康。验证期间不让并发请求
//! 一起涌入：选号时占用名额，名额随 [`TrialLease`] 释放，即这次尝试拿到响应头（成功或失败）
//! 或被放弃时。名额带租约，上游挂住不返回响应头时到期视为空闲。仅存内存。

use parking_lot::Mutex;
use std::sync::Arc;
use std::time::{Duration, Instant};

/// 名额租约时长：超过此时间仍未释放视为空闲
const LEASE: Duration = Duration::from_secs(30);

/// 只因试验名额被占而无号可选时，请求隔多久重新选号
pub(crate) const RECHECK: Duration = Duration::from_secs(1);

#[derive(Default)]
struct SlotState {
    /// 最近一次占用的编号：租约到期后名额可能已被新的占用接手，旧租约释放时据此不去动它
    ticket: u64,
    held_until: Option<Instant>,
}

#[derive(Default)]
pub(crate) struct TrialSlot {
    state: Arc<Mutex<SlotState>>,
}

impl TrialSlot {
    pub(crate) fn is_busy(&self, now: Instant) -> bool {
        self.state
            .lock()
            .held_until
            .is_some_and(|until| now < until)
    }

    /// 占用名额；调用方须先确认名额空闲
    pub(crate) fn claim(&self, now: Instant) -> TrialLease {
        let mut state = self.state.lock();
        state.ticket += 1;
        state.held_until = Some(now + LEASE);
        TrialLease {
            _release_on_drop: Arc::new(LeaseInner {
                state: Arc::clone(&self.state),
                ticket: state.ticket,
            }),
        }
    }
}

/// 占用中的名额；最后一份克隆被丢弃时释放
#[derive(Clone)]
pub struct TrialLease {
    _release_on_drop: Arc<LeaseInner>,
}

struct LeaseInner {
    state: Arc<Mutex<SlotState>>,
    ticket: u64,
}

impl Drop for LeaseInner {
    fn drop(&mut self) {
        let mut state = self.state.lock();
        if state.ticket == self.ticket {
            state.held_until = None;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn claimed_slot_is_busy_until_the_lease_is_dropped() {
        let slot = TrialSlot::default();
        let now = Instant::now();
        assert!(!slot.is_busy(now));

        let lease = slot.claim(now);
        assert!(slot.is_busy(now));

        drop(lease);
        assert!(!slot.is_busy(now));
    }

    #[test]
    fn slot_is_released_only_when_the_last_clone_is_dropped() {
        let slot = TrialSlot::default();
        let now = Instant::now();
        let lease = slot.claim(now);
        let clone = lease.clone();

        drop(lease);
        assert!(slot.is_busy(now));

        drop(clone);
        assert!(!slot.is_busy(now));
    }

    #[test]
    fn lease_expires_when_upstream_never_answers() {
        let slot = TrialSlot::default();
        let now = Instant::now();
        let _lease = slot.claim(now);

        assert!(slot.is_busy(now + LEASE - Duration::from_secs(1)));
        assert!(!slot.is_busy(now + LEASE));
    }

    #[test]
    fn expired_lease_does_not_release_a_newer_claim() {
        let slot = TrialSlot::default();
        let now = Instant::now();
        let stale = slot.claim(now);

        let later = now + LEASE;
        let _fresh = slot.claim(later);
        drop(stale);

        assert!(slot.is_busy(later));
    }
}
