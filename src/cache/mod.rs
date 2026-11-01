// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 上游 prompt cache 命中估算
//!
//! 实测（2026-11-01，`e2e/p0-identity.mjs`、`e2e/p0-ttl.mjs`）：Kiro 的 prompt cache 按
//! 「账号 + 内容前缀」寻址，与 conversationId 无关；TTL 5 分钟，命中即续期。上游不下发
//! 缓存 token 数，只下发本次 credits。
//!
//! 客户端 usage 与用量日志共用 [`estimate_cache_read`] 一个口径，来源按优先级：
//! 1. meteringEvent 带的 cache_read（目前 Kiro 不下发，保留为真值入口）
//! 2. 已标定计费曲线的模型（opus / sonnet / haiku 5.5）按 credits 反推
//! 3. 同账号 5 分钟内处理过的最长公共内容前缀（[`PrefixCache`]）
//!
//! cache_creation 恒为 0：Kiro 不单独计费缓存写入。

mod prefix;

pub use prefix::{PrefixCache, PrefixProbe, PrefixProfile};

use crate::model::usage::{BilledOutput, ReplayedReasoning, infer_cache_read_from_credits};

/// cache_read 的来源
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CacheReadSource {
    /// meteringEvent 下发的真值
    Metering,
    /// 按 credits 与计费曲线反推
    Credits,
    /// 同账号最长公共前缀
    Prefix,
    /// 都没有（如未定账号的失败请求）
    Unknown,
}

/// 本次请求命中上游缓存的输入 token 数
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CacheRead {
    pub tokens: i32,
    pub source: CacheReadSource,
}

/// 估算本次请求的 cache_read（客户端 usage 与用量日志同一口径）。
///
/// 结果截到 `[0, input_tokens - 1]`：至少留 1 个未缓存输入。Claude Code 合并流式 usage 时
/// 只用 message_delta 里大于 0 的字段覆盖 message_start，未缓存输入为 0 会让它保留
/// message_start 的完整输入，与 cache_read 重复计数。
pub fn estimate_cache_read(
    model: &str,
    input_tokens: i32,
    metering_read: Option<i32>,
    credits: Option<f64>,
    replayed: &ReplayedReasoning,
    output: &BilledOutput,
    prefix_read: Option<i32>,
) -> CacheRead {
    let inferred = || {
        credits
            .and_then(|c| infer_cache_read_from_credits(model, input_tokens, replayed, output, c))
    };
    let (tokens, source) = if let Some(read) = metering_read {
        (read, CacheReadSource::Metering)
    } else if let Some(read) = inferred() {
        (read, CacheReadSource::Credits)
    } else if let Some(read) = prefix_read {
        (read, CacheReadSource::Prefix)
    } else {
        (0, CacheReadSource::Unknown)
    };
    let max_read = input_tokens.saturating_sub(1).max(0);
    CacheRead {
        tokens: tokens.clamp(0, max_read),
        source,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const NO_REPLAY: ReplayedReasoning = ReplayedReasoning {
        signature_chars: 0,
        blocks: 0,
    };

    fn plain(tokens: i32) -> BilledOutput {
        BilledOutput {
            tokens,
            native_reasoning_tokens: 0,
            signature_chars: 0,
            signature_blocks: 0,
        }
    }

    #[test]
    fn metering_wins_then_credits_then_prefix() {
        let est = |metering, credits, prefix| {
            estimate_cache_read(
                "claude-opus-5.5",
                16_395,
                metering,
                credits,
                &NO_REPLAY,
                &plain(1),
                prefix,
            )
        };
        assert_eq!(
            est(Some(100), Some(0.09022), Some(50)).source,
            CacheReadSource::Metering
        );

        // 实测全命中的 credits：反推接近全部输入，不受前缀估算压低
        let credits = est(None, Some(0.09022), Some(50));
        assert_eq!(credits.source, CacheReadSource::Credits);
        assert!(credits.tokens > 15_900, "{credits:?}");

        // 未标定模型的 credits 无法反推：落到前缀估算
        let other = estimate_cache_read(
            "claude-sonnet-4.6",
            16_395,
            None,
            Some(0.09),
            &NO_REPLAY,
            &plain(1),
            Some(8_000),
        );
        assert_eq!(
            other,
            CacheRead {
                tokens: 8_000,
                source: CacheReadSource::Prefix
            }
        );

        assert_eq!(
            est(None, None, None),
            CacheRead {
                tokens: 0,
                source: CacheReadSource::Unknown
            }
        );
    }

    #[test]
    fn read_leaves_at_least_one_uncached_token() {
        let read = estimate_cache_read("m", 1_000, Some(5_000), None, &NO_REPLAY, &plain(1), None);
        assert_eq!(read.tokens, 999);

        let tiny = estimate_cache_read("m", 1, None, None, &NO_REPLAY, &plain(1), Some(1));
        assert_eq!(tiny.tokens, 0);
    }
}
