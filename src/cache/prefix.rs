// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 账号级内容前缀命中估算（credits 无法反推时的口径）
//!
//! 把发往上游的请求按「工具定义 → 各轮历史 → 当前消息」切段，逐段求累积摘要：
//! 第 k 段的摘要覆盖第 0..=k 段的全部内容，所以「同账号 5 分钟内见过第 k 段摘要」
//! 等价于「上游在该账号上缓存过这整段前缀」。命中的最长前缀按内容字节占比折算成 token。
//!
//! 同一轮用户消息作为当前消息与之后作为历史时按同一形态（正文、工具结果、图片）求摘要：
//! converter 两次渲染一致时下一轮能接着命中，不一致时上游同样从那里开始重算。
//!
//! ```text
//! 段:      [tools] [history 0] [history 1] ... [current]
//! 摘要:     d0      d1=H(..1)   d2=H(..2)       dn
//! 同账号表: 见过 d0..d2  →  命中前 3 段的字节占比 × input_tokens
//! ```

use std::collections::HashMap;
use std::io::Write;
use std::sync::Arc;
use std::time::{Duration, Instant};

use parking_lot::Mutex;
use serde::Serialize;
use sha2::{Digest, Sha256};

use crate::kiro::model::requests::conversation::{
    AssistantMessage, ConversationState, KiroImage, Message,
};
use crate::kiro::model::requests::tool::ToolResult;

/// 上游 prompt cache TTL（实测 5 分钟，命中即续期）
const PREFIX_TTL: Duration = Duration::from_secs(5 * 60);
/// 前缀表容量上限（约 60 字节 / 条）：超出时先清理过期条目，仍超出再淘汰最旧的一成
const MAX_ENTRIES: usize = 300_000;

type SegmentDigest = [u8; 16];

/// 一次请求的内容前缀画像
#[derive(Debug, Clone)]
pub struct PrefixProfile {
    /// 各段的累积摘要
    digests: Vec<SegmentDigest>,
    /// 各段末尾的累积内容字节数（图片、推理签名不计，与本地输入 token 统计口径一致）
    cumulative_bytes: Vec<u64>,
}

impl PrefixProfile {
    pub fn from_state(state: &ConversationState) -> serde_json::Result<Self> {
        let mut builder = ProfileBuilder::default();
        let current = &state.current_message.user_input_message;

        builder.segment(b'T', |feed| {
            feed.hashed(&current.model_id)?;
            feed.weighted(&current.user_input_message_context.tools)
        })?;
        for message in &state.history {
            match message {
                Message::User(user) => {
                    let msg = &user.user_input_message;
                    builder.user_turn(
                        &msg.content,
                        &msg.user_input_message_context.tool_results,
                        &msg.images,
                    )?;
                }
                Message::Assistant(assistant) => {
                    builder.assistant_turn(&assistant.assistant_response_message)?;
                }
            }
        }
        builder.user_turn(
            &current.content,
            &current.user_input_message_context.tool_results,
            &current.images,
        )?;

        Ok(builder.finish())
    }

    fn total_bytes(&self) -> u64 {
        self.cumulative_bytes.last().copied().unwrap_or(0)
    }
}

#[derive(Default)]
struct ProfileBuilder {
    hasher: Sha256,
    bytes: u64,
    profile: Vec<(SegmentDigest, u64)>,
}

impl ProfileBuilder {
    fn segment(
        &mut self,
        tag: u8,
        write: impl FnOnce(&mut Feed<'_>) -> serde_json::Result<()>,
    ) -> serde_json::Result<()> {
        self.hasher.update([tag]);
        write(&mut Feed {
            hasher: &mut self.hasher,
            bytes: &mut self.bytes,
        })?;
        let full: [u8; 32] = self.hasher.clone().finalize().into();
        let mut digest = SegmentDigest::default();
        let len = digest.len();
        digest.copy_from_slice(&full[..len]);
        self.profile.push((digest, self.bytes));
        Ok(())
    }

    fn user_turn(
        &mut self,
        content: &str,
        tool_results: &[ToolResult],
        images: &[KiroImage],
    ) -> serde_json::Result<()> {
        self.segment(b'U', |feed| {
            feed.weighted(content)?;
            feed.weighted(tool_results)?;
            feed.hashed(images)
        })
    }

    fn assistant_turn(&mut self, message: &AssistantMessage) -> serde_json::Result<()> {
        self.segment(b'A', |feed| {
            feed.weighted(&message.content)?;
            feed.weighted(&message.tool_uses)?;
            feed.hashed(&message.reasoning_content)
        })
    }

    fn finish(self) -> PrefixProfile {
        let (digests, cumulative_bytes) = self.profile.into_iter().unzip();
        PrefixProfile {
            digests,
            cumulative_bytes,
        }
    }
}

/// 段内容写入口：JSON 值首尾自界定，依次写入的多个值不会拼出歧义
struct Feed<'a> {
    hasher: &'a mut Sha256,
    bytes: &'a mut u64,
}

impl Feed<'_> {
    /// 计入摘要与内容字节
    fn weighted<T: Serialize + ?Sized>(&mut self, value: &T) -> serde_json::Result<()> {
        let mut sink = Sink {
            hasher: &mut *self.hasher,
            bytes: Some(&mut *self.bytes),
        };
        serde_json::to_writer(&mut sink, value)
    }

    /// 只计入摘要（图片、推理签名：本地输入 token 统计不含它们）
    fn hashed<T: Serialize + ?Sized>(&mut self, value: &T) -> serde_json::Result<()> {
        let mut sink = Sink {
            hasher: &mut *self.hasher,
            bytes: None,
        };
        serde_json::to_writer(&mut sink, value)
    }
}

struct Sink<'a> {
    hasher: &'a mut Sha256,
    bytes: Option<&'a mut u64>,
}

impl Write for Sink<'_> {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.hasher.update(buf);
        if let Some(bytes) = self.bytes.as_deref_mut() {
            *bytes += buf.len() as u64;
        }
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// 各账号近 5 分钟处理过的前缀摘要
#[derive(Default)]
pub struct PrefixCache {
    entries: Mutex<HashMap<(u64, SegmentDigest), Instant>>,
}

impl PrefixCache {
    pub fn new() -> Self {
        Self::default()
    }

    /// 结算一次已由 `account` 处理的请求：返回按命中前缀折算的 cache_read，
    /// 并把本次的全部前缀登记为该账号已缓存（命中的续期）
    fn settle_at(
        &self,
        account: u64,
        profile: &PrefixProfile,
        input_tokens: i32,
        now: Instant,
    ) -> i32 {
        let mut entries = self.entries.lock();
        let is_cached = |digest: &SegmentDigest| {
            entries
                .get(&(account, *digest))
                .is_some_and(|&at| is_live(at, now))
        };
        let matched_bytes = profile
            .digests
            .iter()
            .rposition(is_cached)
            .map_or(0, |k| profile.cumulative_bytes[k]);

        for digest in &profile.digests {
            entries.insert((account, *digest), now);
        }
        enforce_capacity(&mut entries, now);
        drop(entries);

        let total = profile.total_bytes();
        if total == 0 {
            return 0;
        }
        let read =
            u128::from(input_tokens.max(0) as u64) * u128::from(matched_bytes) / u128::from(total);
        read as i32
    }
}

fn is_live(at: Instant, now: Instant) -> bool {
    now.saturating_duration_since(at) < PREFIX_TTL
}

fn enforce_capacity(entries: &mut HashMap<(u64, SegmentDigest), Instant>, now: Instant) {
    if entries.len() <= MAX_ENTRIES {
        return;
    }
    entries.retain(|_, at| is_live(*at, now));
    if entries.len() <= MAX_ENTRIES {
        return;
    }
    let mut times: Vec<Instant> = entries.values().copied().collect();
    let cutoff_index = times.len() / 10;
    let (_, cutoff, _) = times.select_nth_unstable(cutoff_index);
    let cutoff = *cutoff;
    entries.retain(|_, at| *at > cutoff);
}

/// 一次请求的前缀观测：handler 构建，响应结束、账号确定后结算一次
pub struct PrefixProbe {
    cache: Arc<PrefixCache>,
    profile: PrefixProfile,
}

impl PrefixProbe {
    pub fn new(cache: Arc<PrefixCache>, profile: PrefixProfile) -> Self {
        Self { cache, profile }
    }

    /// 见 [`PrefixCache`]：返回本次按前缀命中折算的 cache_read
    pub fn settle(&self, account: u64, input_tokens: i32) -> i32 {
        self.cache
            .settle_at(account, &self.profile, input_tokens, Instant::now())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kiro::model::requests::conversation::{
        CurrentMessage, HistoryAssistantMessage, HistoryUserMessage, UserInputMessage,
    };
    use crate::kiro::model::requests::tool::{InputSchema, Tool, ToolSpecification};

    const MODEL: &str = "claude-sonnet-4.6";

    /// 交替的 user / assistant 历史 + 当前消息
    fn state(turns: &[&str], current: &str) -> ConversationState {
        let history = turns
            .iter()
            .enumerate()
            .map(|(i, text)| {
                if i % 2 == 0 {
                    Message::User(HistoryUserMessage::new(*text, MODEL))
                } else {
                    Message::Assistant(HistoryAssistantMessage::new(*text))
                }
            })
            .collect();
        ConversationState::new("c")
            .with_history(history)
            .with_current_message(CurrentMessage::new(UserInputMessage::new(current, MODEL)))
    }

    fn profile(turns: &[&str], current: &str) -> PrefixProfile {
        PrefixProfile::from_state(&state(turns, current)).unwrap()
    }

    #[test]
    fn next_turn_hits_previous_prompt_including_its_current_message() {
        let cache = PrefixCache::new();
        let now = Instant::now();
        let system = "s".repeat(9_000);
        let first = profile(&[&system, "ack"], "question-1");
        assert_eq!(cache.settle_at(1, &first, 1_000, now), 0, "冷启动");

        // 下一轮：上一轮的当前消息成为历史，同形态求摘要
        let second = profile(&[&system, "ack", "question-1", "answer-1"], "question-2");
        let read = cache.settle_at(1, &second, 1_000, now);
        let matched = second.cumulative_bytes[3] as f64 / second.total_bytes() as f64;
        assert_eq!(read, (1_000.0 * matched) as i32);
        assert!(read > 900, "{read}");
    }

    #[test]
    fn other_account_and_expired_entries_miss() {
        let cache = PrefixCache::new();
        let now = Instant::now();
        let p = profile(&["system", "ack"], "q");
        cache.settle_at(1, &p, 100, now);

        assert_eq!(cache.settle_at(2, &p, 100, now), 0, "缓存不跨账号");
        assert_eq!(cache.settle_at(1, &p, 100, now + PREFIX_TTL), 0, "已过期");
    }

    #[test]
    fn hit_refreshes_ttl() {
        let cache = PrefixCache::new();
        let t0 = Instant::now();
        let p = profile(&["system", "ack"], "q");
        cache.settle_at(1, &p, 100, t0);

        let t4 = t0 + Duration::from_secs(4 * 60);
        assert!(cache.settle_at(1, &p, 100, t4) > 0);
        assert!(cache.settle_at(1, &p, 100, t4 + Duration::from_secs(4 * 60)) > 0);
    }

    #[test]
    fn divergence_stops_the_match() {
        let cache = PrefixCache::new();
        let now = Instant::now();
        let base = "b".repeat(1_000);
        cache.settle_at(1, &profile(&[&base, "ack", "old-question"], "x"), 100, now);

        // 第 3 段不同：只命中前两段
        let p = profile(&[&base, "ack", "new-question"], "x");
        let read = cache.settle_at(1, &p, 100, now);
        let expected = 100 * p.cumulative_bytes[2] / p.total_bytes();
        assert_eq!(read as u64, expected);
    }

    #[test]
    fn interleaved_sessions_on_one_account_keep_their_prefixes() {
        let cache = PrefixCache::new();
        let now = Instant::now();
        let a = profile(&["system", "ack", "task-a"], "a1");
        let b = profile(&["system", "ack", "task-b"], "b1");
        cache.settle_at(1, &a, 100, now);
        cache.settle_at(1, &b, 100, now);

        // A 回来时整段仍在（旧指纹表在分叉处截断，会丢掉 A 的历史）
        assert_eq!(cache.settle_at(1, &a, 100, now), 100);
    }

    #[test]
    fn images_change_digest_but_not_weight() {
        let plain = profile(&["system"], "q");
        let mut with_image = state(&["system"], "q");
        with_image.current_message.user_input_message.images =
            vec![KiroImage::from_base64("png", "A".repeat(10_000))];
        let with_image = PrefixProfile::from_state(&with_image).unwrap();

        assert_ne!(plain.digests.last(), with_image.digests.last());
        assert_eq!(plain.total_bytes(), with_image.total_bytes());
    }

    #[test]
    fn tools_change_invalidates_everything() {
        let cache = PrefixCache::new();
        let now = Instant::now();
        let p = profile(&["system", "ack"], "q");
        cache.settle_at(1, &p, 100, now);

        let mut other_tools = state(&["system", "ack"], "q");
        other_tools
            .current_message
            .user_input_message
            .user_input_message_context
            .tools = vec![Tool {
            tool_specification: ToolSpecification {
                name: "Read".to_string(),
                description: String::new(),
                input_schema: InputSchema::default(),
            },
        }];
        let other_tools = PrefixProfile::from_state(&other_tools).unwrap();
        assert_eq!(cache.settle_at(1, &other_tools, 100, now), 0);
    }
}
