// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 会话系统块基线：把会话中途新增的系统块分流出 history[0]
//!
//! history[0] 的缓存 key 是整段稳定系统内容的哈希。Claude Code 会在会话中途向 system 区追加新块
//! （按路径加载的 `~/.claude/rules/*.md`、各类状态通知……），每追加一次，key 变化，上游前缀缓存
//! 整段失效，下一轮要把整个上下文按未缓存重新计费（实测一次重置多花约 0.5 credits）。
//! `prompt::is_dynamic_hook_injection` 靠文本前缀识别已知的动态块，追不上 CC 的新花样；
//! 这里改用「是不是会话开始时就有的块」来判断，与块的文本无关。
//!
//! 做法：按会话记录基线（首次见到的稳定块指纹多重集）。之后的请求只要**包含全部基线块**，
//! 多出来的块就视为会话中途新增，不进 history[0]，由调用方放到当前消息开头——模型仍可见，
//! 但不参与缓存哈希。
//!
//! 不满足「包含全部基线块」的请求（子代理、压缩后的请求、已有块被原地改写）不被分流，
//! 并登记为该会话的新基线，行为与引入本模块之前一致。同一会话保留少量基线，
//! 避免子代理等辅助请求占掉主请求的基线。
//!
//! 误判防护：多出来的块总字节数必须不超过所匹配的基线本身的字节数。否则会出现
//! 「先到的是系统提示很短的辅助请求，随后到的主请求恰好包含它」，把主请求整份系统提示
//! 当作新增块搬走的情况。
//!
//! 基线随会话存放在 [`super::session_store::SessionState`]。

use std::time::Instant;

use super::drift_diag::{Fingerprint, fingerprint};
use super::prompt::normalize_billing_header;

/// 单个会话最多保留的基线数（主请求 + 若干种辅助 / 子代理请求）
const MAX_BASELINES_PER_SESSION: usize = 4;

struct Baseline {
    fps: Vec<Fingerprint>,
    /// 基线各块字节数之和，用于误判防护
    bytes: usize,
    last_used: Instant,
}

/// 一个会话的系统块基线
#[derive(Default)]
pub(super) struct Baselines(Vec<Baseline>);

/// 与 history[0] 的 key 同口径：先规范化 `cch=`，避免计费哈希的逐轮变化被当成新块
fn block_fingerprint(block: &str) -> Fingerprint {
    // 只有含计费头 `cch=` 的块需要规范化（须拷贝一份来改写），其余直接借用，
    // 避免每次请求把整份系统提示多拷贝一遍
    if block.contains("cch=") {
        fingerprint(&normalize_billing_header(block.to_string()))
    } else {
        fingerprint(block)
    }
}

/// 基线是 `current` 的子多重集时，返回 `current` 中多出来的块的下标；否则返回 `None`
fn extras_of(baseline: &[Fingerprint], current: &[Fingerprint]) -> Option<Vec<usize>> {
    let mut remaining: Vec<Option<Fingerprint>> = baseline.iter().copied().map(Some).collect();
    let mut extras = Vec::new();
    for (i, fp) in current.iter().enumerate() {
        match remaining.iter_mut().find(|slot| **slot == Some(*fp)) {
            Some(slot) => *slot = None,
            None => extras.push(i),
        }
    }
    remaining.iter().all(Option::is_none).then_some(extras)
}

/// 把稳定系统块拆成 `(留在 history[0] 的块, 会话中途新增的块)`，两部分各自保持原顺序。
///
/// 会话首次出现（或没有任何基线匹配）时，全部块视为稳定，并登记为新基线。
pub(super) fn split_new_blocks<'a>(
    baselines: &mut Baselines,
    session_id: &str,
    blocks: &[&'a str],
) -> (Vec<&'a str>, Vec<&'a str>) {
    if blocks.is_empty() {
        return (Vec::new(), Vec::new());
    }
    let fps: Vec<Fingerprint> = blocks.iter().map(|b| block_fingerprint(b)).collect();
    let known = &mut baselines.0;

    // 多个基线都匹配时取最大的（多出来的块最少）
    let best = known
        .iter()
        .enumerate()
        .filter_map(|(i, b)| {
            let extras = extras_of(&b.fps, &fps)?;
            let extra_bytes: usize = extras.iter().map(|&k| fps[k].1).sum();
            (extra_bytes <= b.bytes).then_some((i, b.bytes, extras))
        })
        .max_by_key(|(_, bytes, _)| *bytes);

    if let Some((i, _, extras)) = best {
        known[i].last_used = Instant::now();
        if extras.is_empty() {
            return (blocks.to_vec(), Vec::new());
        }
        let mut is_extra = vec![false; blocks.len()];
        for &k in &extras {
            is_extra[k] = true;
        }
        let mut stable = Vec::with_capacity(blocks.len() - extras.len());
        let mut added = Vec::with_capacity(extras.len());
        for (block, extra) in blocks.iter().zip(is_extra) {
            if extra {
                added.push(*block);
            } else {
                stable.push(*block);
            }
        }
        return (stable, added);
    }

    tracing::info!(
        session = %session_id,
        blocks = blocks.len(),
        known_baselines = known.len(),
        "[baseline] 登记新的系统块基线"
    );
    if known.len() >= MAX_BASELINES_PER_SESSION
        && let Some(oldest) = known
            .iter()
            .enumerate()
            .min_by_key(|(_, b)| b.last_used)
            .map(|(i, _)| i)
    {
        known.swap_remove(oldest);
    }
    known.push(Baseline {
        bytes: fps.iter().map(|f| f.1).sum(),
        fps,
        last_used: Instant::now(),
    });
    (blocks.to_vec(), Vec::new())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 日志用的会话名
    fn sid(name: &str) -> String {
        format!("baseline-test-{name}")
    }

    fn big(tag: &str, n: usize) -> String {
        format!("{tag}:{}", "x".repeat(n))
    }

    #[test]
    fn first_request_registers_baseline_and_diverts_nothing() {
        let bl = &mut Baselines::default();
        let s = sid("first");
        let (a, b, c) = (big("a", 100), big("b", 100), big("c", 100));
        let (stable, added) = split_new_blocks(bl, &s, &[&a, &b, &c]);
        assert_eq!(stable, [a.as_str(), b.as_str(), c.as_str()]);
        assert!(added.is_empty());
    }

    #[test]
    fn block_added_mid_session_is_diverted_wherever_it_lands() {
        let bl = &mut Baselines::default();
        let s = sid("added");
        let (a, b, c) = (big("a", 1000), big("b", 1000), big("c", 1000));
        let rule = big("Contents of rules", 200);
        split_new_blocks(bl, &s, &[&a, &b, &c]);

        // 插在中间（与实测一致：新块不在末尾）
        let (stable, added) = split_new_blocks(bl, &s, &[&a, &b, &rule, &c]);
        assert_eq!(stable, [a.as_str(), b.as_str(), c.as_str()]);
        assert_eq!(added, [rule.as_str()]);

        // 之后每轮都带着它：结果一致，history[0] 的输入不再变化
        let (stable2, added2) = split_new_blocks(bl, &s, &[&a, &b, &rule, &c]);
        assert_eq!(stable2, stable);
        assert_eq!(added2, added);

        // 再来一个新块、顺序被打乱：仍只分流新块
        let rule2 = big("another", 150);
        let (stable3, added3) = split_new_blocks(bl, &s, &[&rule2, &c, &a, &rule, &b]);
        assert_eq!(stable3, [c.as_str(), a.as_str(), b.as_str()]);
        assert_eq!(added3, [rule2.as_str(), rule.as_str()]);
    }

    #[test]
    fn rewritten_block_is_not_a_superset_and_becomes_new_baseline() {
        let bl = &mut Baselines::default();
        let s = sid("rewritten");
        let (a, b) = (big("a", 1000), big("b", 1000));
        let b2 = big("b-edited", 1000);
        split_new_blocks(bl, &s, &[&a, &b]);

        let (stable, added) = split_new_blocks(bl, &s, &[&a, &b2]);
        assert_eq!(stable, [a.as_str(), b2.as_str()]);
        assert!(added.is_empty());

        // 新基线生效：之后在它基础上新增的块被分流
        let rule = big("rule", 100);
        let (stable, added) = split_new_blocks(bl, &s, &[&a, &b2, &rule]);
        assert_eq!(stable, [a.as_str(), b2.as_str()]);
        assert_eq!(added, [rule.as_str()]);
    }

    #[test]
    fn unrelated_request_does_not_evict_main_baseline() {
        let bl = &mut Baselines::default();
        let s = sid("subagent");
        let (a, b) = (big("a", 5000), big("b", 5000));
        let sub = big("subagent-system", 800);
        let rule = big("rule", 300);
        split_new_blocks(bl, &s, &[&a, &b]);

        // 子代理请求：不包含主基线，原样放行并登记自己的基线
        let (stable, added) = split_new_blocks(bl, &s, &[&sub]);
        assert_eq!(stable, [sub.as_str()]);
        assert!(added.is_empty());

        // 回到主请求，新增块照常被分流
        let (stable, added) = split_new_blocks(bl, &s, &[&a, &b, &rule]);
        assert_eq!(stable, [a.as_str(), b.as_str()]);
        assert_eq!(added, [rule.as_str()]);
    }

    #[test]
    fn small_baseline_cannot_swallow_a_larger_main_prompt() {
        let bl = &mut Baselines::default();
        let s = sid("small-first");
        let ident = big("identity", 50);
        let main_prompt = big("main-system-prompt", 20_000);
        // 先到的是系统提示很短的辅助请求
        split_new_blocks(bl, &s, &[&ident]);

        // 主请求包含辅助请求的块，但多出来的部分远大于基线：不能当成新增块搬走
        let (stable, added) = split_new_blocks(bl, &s, &[&ident, &main_prompt]);
        assert_eq!(stable, [ident.as_str(), main_prompt.as_str()]);
        assert!(added.is_empty());
    }

    #[test]
    fn billing_header_cch_changes_are_not_new_blocks() {
        let bl = &mut Baselines::default();
        let s = sid("cch");
        let h1 = "x-anthropic-billing-header: cc_version=2.1; cch=11111;";
        let h2 = "x-anthropic-billing-header: cc_version=2.1; cch=22222;";
        let body = big("body", 500);
        split_new_blocks(bl, &s, &[h1, &body]);
        let (stable, added) = split_new_blocks(bl, &s, &[h2, &body]);
        assert_eq!(stable.len(), 2);
        assert!(added.is_empty());
    }

    #[test]
    fn duplicate_blocks_are_matched_by_count() {
        let bl = &mut Baselines::default();
        let s = sid("dup");
        let a = big("a", 1000);
        split_new_blocks(bl, &s, &[&a]);
        // 基线里只有一份 a，请求里出现第二份相同内容的块，算新增
        let (stable, added) = split_new_blocks(bl, &s, &[&a, &a]);
        assert_eq!(stable, [a.as_str()]);
        assert_eq!(added, [a.as_str()]);
    }

    #[test]
    fn baselines_per_session_are_bounded() {
        let bl = &mut Baselines::default();
        let s = sid("bounded");
        for i in 0..(MAX_BASELINES_PER_SESSION + 3) {
            let blk = big(&format!("distinct-{i}"), 100);
            split_new_blocks(bl, &s, &[&blk]);
        }
        assert!(bl.0.len() <= MAX_BASELINES_PER_SESSION);
    }
}
