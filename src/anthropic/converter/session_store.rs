// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 会话级转换状态
//!
//! 转换本身不持有全局状态：跨请求需要记住的东西（系统块基线、见过的 history[0]、漂移诊断快照）
//! 都按 conversationId 存在这里，由 handler 层持有一个 [`SessionStore`] 传给 `convert_request`。
//!
//! 状态只在内存里，空闲 [`SESSION_IDLE_TTL`] 后丢弃。丢失的代价是该会话下一次请求按新会话
//! 处理（重新登记基线），上游缓存那时多半也已过期（实测 TTL 5 分钟）。

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use parking_lot::Mutex;

use super::baseline::Baselines;
use super::drift_diag::DriftDiag;

/// 会话状态空闲多久后丢弃。比上游缓存 TTL（实测 5 分钟）宽松：其它模型的 TTL 未实测，
/// 状态本身很小，多留一会儿只占内存
const SESSION_IDLE_TTL: Duration = Duration::from_secs(60 * 60);
/// 会话数上限：超出时先清理过期会话，仍超出再淘汰最久未用的
const MAX_SESSIONS: usize = 10_000;
/// 每个会话记住的 history[0] 摘要数（主请求 + 几类辅助 / 子代理请求 + 少量漂移）
const MAX_SEEN_HISTORY0: usize = 8;

/// 一个会话的转换状态
#[derive(Default)]
pub(super) struct SessionState {
    /// 系统块基线（见 `baseline` 模块）
    pub(super) baselines: Baselines,
    /// history[0] 漂移诊断
    pub(super) drift: DriftDiag,
    /// 最近见过的 history[0] 摘要，最近的在末尾
    seen_history0: Vec<[u8; 16]>,
}

impl SessionState {
    /// 记录本次的 history[0] 摘要；此前没见过时返回 `true`
    pub(super) fn observe_history0(&mut self, digest: [u8; 16]) -> bool {
        let seen = &mut self.seen_history0;
        let first = match seen.iter().position(|d| *d == digest) {
            Some(index) => {
                seen.remove(index);
                false
            }
            None => true,
        };
        seen.push(digest);
        if seen.len() > MAX_SEEN_HISTORY0 {
            seen.remove(0);
        }
        first
    }
}

struct Entry {
    state: Arc<Mutex<SessionState>>,
    last_used: Instant,
}

/// conversationId → 会话转换状态
#[derive(Default)]
pub struct SessionStore {
    sessions: Mutex<HashMap<String, Entry>>,
}

impl SessionStore {
    pub fn new() -> Self {
        Self::default()
    }

    /// 取会话状态（没有则新建）。返回的锁只属于这个会话，同一会话的并发请求依次转换
    pub(super) fn session(&self, conversation_id: &str) -> Arc<Mutex<SessionState>> {
        self.session_at(conversation_id, Instant::now())
    }

    fn session_at(&self, conversation_id: &str, now: Instant) -> Arc<Mutex<SessionState>> {
        let mut sessions = self.sessions.lock();
        if let Some(entry) = sessions
            .get_mut(conversation_id)
            .filter(|e| is_live(e.last_used, now))
        {
            entry.last_used = now;
            return entry.state.clone();
        }

        let state = Arc::new(Mutex::new(SessionState::default()));
        sessions.insert(
            conversation_id.to_string(),
            Entry {
                state: state.clone(),
                last_used: now,
            },
        );
        enforce_capacity(&mut sessions, now);
        state
    }
}

fn is_live(last_used: Instant, now: Instant) -> bool {
    now.saturating_duration_since(last_used) < SESSION_IDLE_TTL
}

fn enforce_capacity(sessions: &mut HashMap<String, Entry>, now: Instant) {
    if sessions.len() <= MAX_SESSIONS {
        return;
    }
    sessions.retain(|_, e| is_live(e.last_used, now));
    while sessions.len() > MAX_SESSIONS {
        let Some(oldest) = sessions
            .iter()
            .min_by_key(|(_, e)| e.last_used)
            .map(|(k, _)| k.clone())
        else {
            break;
        };
        sessions.remove(&oldest);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn same_conversation_shares_state_until_idle_ttl() {
        let store = SessionStore::new();
        let t0 = Instant::now();
        let first = store.session_at("c", t0);
        assert!(Arc::ptr_eq(&first, &store.session_at("c", t0)));
        assert!(!Arc::ptr_eq(&first, &store.session_at("other", t0)));

        let later = t0 + SESSION_IDLE_TTL;
        assert!(!Arc::ptr_eq(&first, &store.session_at("c", later)));
    }

    #[test]
    fn capacity_evicts_least_recently_used() {
        let store = SessionStore::new();
        let t0 = Instant::now();
        for i in 0..=MAX_SESSIONS {
            store.session_at(&format!("c{i}"), t0 + Duration::from_micros(i as u64));
        }
        let sessions = store.sessions.lock();
        assert_eq!(sessions.len(), MAX_SESSIONS);
        assert!(!sessions.contains_key("c0"));
    }

    #[test]
    fn history0_digest_is_first_only_once_and_bounded() {
        let mut state = SessionState::default();
        assert!(state.observe_history0([1; 16]));
        assert!(!state.observe_history0([1; 16]));

        for i in 2..=(MAX_SEEN_HISTORY0 as u8 + 1) {
            assert!(state.observe_history0([i; 16]));
        }
        // [1] 已被挤出
        assert!(state.observe_history0([1; 16]));
        assert_eq!(state.seen_history0.len(), MAX_SEEN_HISTORY0);
    }
}
