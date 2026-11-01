// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! history[0] 漂移诊断（INFO 日志，不影响请求内容）
//!
//! 同一会话内 history[0] 的缓存 key 变化（`[exp2] history[0] first`）意味着上游前缀缓存
//! 整段失效。本模块记录每个会话最近几类请求的稳定系统块指纹，key 变化时列出相对
//! 「最相似的一类」新增 / 消失的系统块（序号、长度、前 80 个字符），用来识别 CC 追加了
//! 什么内容、该不该归入动态块（见 `prompt::is_dynamic_hook_injection`）。
//!
//! 同一会话（同一 `metadata.user_id`）里会夹杂辅助请求（标题生成、额度探测、子代理等），
//! 它们的系统提示与主对话几乎没有重叠、各自有独立的 history[0] 缓存。若只保留「上一次」
//! 一份快照，主请求首次出现时会被拿去和辅助请求比较，产生「新增 4 块 / 消失 2 块」的误报。
//! 所以每个会话保留至多 `MAX_SNAPSHOTS_PER_SESSION` 份快照，按重叠字节数匹配；与所有
//! 快照都几乎无重叠时单独标注为「新请求类型」，不再当作漂移。
//!
//! INFO 记录诊断摘要和系统块开头原文预览（80 / 40 个字符）。
//! 不依赖 Admin 设置页的 `debugLogs` 开关；INFO 未启用时跳过诊断。
//! 只在 key 变化、或分流块数量变化时各输出一行。
//!
//! 状态随会话存放在 [`super::session_store::SessionState`]。

use std::hash::{Hash, Hasher};

use super::prompt::normalize_billing_header;

/// 预览长度（字符数）
const PREVIEW_CHARS: usize = 80;
/// 分流块预览长度（字符数）
const DIVERTED_PREVIEW_CHARS: usize = 40;
/// 单次日志最多列出的新增 / 消失块数
const MAX_LISTED: usize = 8;
/// 每个会话最多保留的快照数（主对话 + 若干类辅助 / 子代理请求）
const MAX_SNAPSHOTS_PER_SESSION: usize = 4;

/// 单个系统块的指纹：内容哈希 + 字节长度
pub(super) type Fingerprint = (u64, usize);

/// 一个会话的漂移诊断状态
#[derive(Default)]
pub(super) struct DriftDiag {
    /// 最近几类请求的稳定系统块指纹
    snapshots: Vec<Vec<Fingerprint>>,
    /// 上一次被分流的动态块数量，用于只在数量变化时写日志
    diverted: usize,
}

/// 一个新增块：在当前系统区中的序号、字节长度、预览
#[derive(Debug, PartialEq, Eq)]
struct AddedBlock {
    index: usize,
    len: usize,
    preview: String,
}

pub(super) fn fingerprint(block: &str) -> Fingerprint {
    let mut h = std::collections::hash_map::DefaultHasher::new();
    block.hash(&mut h);
    (h.finish(), block.len())
}

/// 与上一次比较：返回当前新增的块，以及上一次有、这次没有的块长度。
/// 同一指纹出现多次时按次数配对（多重集差）。
fn diff_blocks(prev: &[Fingerprint], current: &[String]) -> (Vec<AddedBlock>, Vec<usize>) {
    let mut remaining: Vec<Option<Fingerprint>> = prev.iter().copied().map(Some).collect();
    let mut added = Vec::new();
    for (index, block) in current.iter().enumerate() {
        let fp = fingerprint(block);
        if let Some(slot) = remaining.iter_mut().find(|s| **s == Some(fp)) {
            *slot = None;
        } else {
            added.push(AddedBlock {
                index,
                len: block.len(),
                preview: preview(block, PREVIEW_CHARS),
            });
        }
    }
    let removed = remaining
        .into_iter()
        .flatten()
        .map(|(_, len)| len)
        .collect();
    (added, removed)
}

/// 取块开头 `max_chars` 个字符作预览。换行等控制字符不在这里转义：
/// 日志侧统一用 `{:?}` 格式化，会自行转义，避免二次转义出现 `\\n`。
fn preview(block: &str, max_chars: usize) -> String {
    block.trim_start().chars().take(max_chars).collect()
}

/// 与某份快照的匹配结果
enum Matched {
    /// 该会话此前没有任何快照
    First,
    /// 与某份快照重叠足够多，视为同一类请求；携带被替换掉的旧快照
    Related(Vec<Fingerprint>),
    /// 与已有快照几乎无重叠；携带已有快照数
    Unrelated(usize),
}

fn total_len(fps: &[Fingerprint]) -> usize {
    fps.iter().map(|f| f.1).sum()
}

/// 两组指纹按多重集求交，返回共有块的总字节数
fn shared_bytes(prev: &[Fingerprint], cur: &[Fingerprint]) -> usize {
    let mut remaining: Vec<Option<Fingerprint>> = prev.iter().copied().map(Some).collect();
    let mut shared = 0;
    for fp in cur {
        if let Some(slot) = remaining.iter_mut().find(|s| **s == Some(*fp)) {
            *slot = None;
            shared += fp.1;
        }
    }
    shared
}

/// 共有字节数不少于较小一方总字节数的 1/3，视为同一类请求。
/// 真漂移（追加 / 改写少数块）重叠接近 100%；辅助请求（约 1KB）对主对话（数十 KB）接近 0。
fn is_related(prev: &[Fingerprint], cur: &[Fingerprint], shared: usize) -> bool {
    shared * 3 >= total_len(prev).min(total_len(cur))
}

/// 记录本次请求的稳定系统块；`key_changed` 为 true（history[0] 缓存 key 首次出现）且该会话
/// 之前有记录时：与某份快照足够相似则输出新增 / 消失块，与所有快照都几乎无重叠则输出
/// 「新请求类型」提示（多为辅助 / 子代理请求，不是漂移）。
pub(super) fn observe(
    diag: &mut DriftDiag,
    session_id: &str,
    stable_blocks: &[&str],
    key_changed: bool,
) {
    if !tracing::enabled!(tracing::Level::INFO) {
        return;
    }
    // 与 history[0] 的 key 同口径：先规范化 cch，避免计费哈希的逐轮变化被当成漂移
    let current: Vec<String> = stable_blocks
        .iter()
        .map(|b| normalize_billing_header((*b).to_string()))
        .collect();
    let fps: Vec<Fingerprint> = current.iter().map(|b| fingerprint(b)).collect();
    let cur_total = total_len(&fps);

    let matched = {
        let snaps = &mut diag.snapshots;
        let best = snaps
            .iter()
            .enumerate()
            .map(|(i, p)| (i, shared_bytes(p, &fps)))
            .max_by_key(|&(_, shared)| shared);
        match best {
            Some((i, shared)) if is_related(&snaps[i], &fps, shared) => {
                Matched::Related(std::mem::replace(&mut snaps[i], fps))
            }
            Some(_) => {
                let known = snaps.len();
                if known >= MAX_SNAPSHOTS_PER_SESSION {
                    snaps.remove(0);
                }
                snaps.push(fps);
                Matched::Unrelated(known)
            }
            None => {
                snaps.push(fps);
                Matched::First
            }
        }
    };

    if !key_changed {
        return;
    }
    match matched {
        Matched::First => {}
        Matched::Unrelated(known) => {
            tracing::info!(
                session = %session_id,
                blocks = current.len(),
                total_bytes = cur_total,
                known_kinds = known,
                "[drift] 会话内出现与已知系统块几乎无重叠的新请求类型（多为辅助 / 子代理请求，\
                 各自独立缓存 history[0]，不是漂移；主请求的系统提示整段被替换时也会走到这里）"
            );
        }
        Matched::Related(prev) => {
            let (added, removed) = diff_blocks(&prev, &current);
            let listed: Vec<String> = added
                .iter()
                .take(MAX_LISTED)
                .map(|b| format!("#{}({}B) {:?}", b.index, b.len, b.preview))
                .collect();
            tracing::info!(
                session = %session_id,
                blocks = current.len(),
                added = added.len(),
                removed = removed.len(),
                removed_lens = ?removed.iter().take(MAX_LISTED).collect::<Vec<_>>(),
                "[drift] history[0] 缓存 key 变化，相对上一次的系统块差异：新增 [{}]",
                listed.join(" | ")
            );
        }
    }
}

/// 记录本次请求中被分流为动态块（不进 history[0]）的系统块：INFO 记录块数、各块字节长度
/// 与前 40 个字符的原文预览。
/// 用来确认分流规则确实命中（例如任务工具提醒）。同一会话内只在分流块数量变化时输出，
/// 避免动态块持续存在时每轮刷屏；无动态块且数量未变化时不输出。
pub(super) fn log_diverted(diag: &mut DriftDiag, session_id: &str, dynamic_blocks: &[&str]) {
    if !tracing::enabled!(tracing::Level::INFO) {
        return;
    }
    let prev = std::mem::replace(&mut diag.diverted, dynamic_blocks.len());
    if prev == dynamic_blocks.len() {
        return;
    }
    if dynamic_blocks.is_empty() {
        return;
    }
    let listed: Vec<String> = dynamic_blocks
        .iter()
        .take(MAX_LISTED)
        .map(|b| format!("({}B) {:?}", b.len(), preview(b, DIVERTED_PREVIEW_CHARS)))
        .collect();
    tracing::info!(
        session = %session_id,
        diverted = dynamic_blocks.len(),
        "[drift] 动态系统块已分流出 history[0]：[{}]",
        listed.join(" | ")
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fps(blocks: &[&str]) -> Vec<Fingerprint> {
        blocks.iter().map(|b| fingerprint(b)).collect()
    }

    fn owned(blocks: &[&str]) -> Vec<String> {
        blocks.iter().map(|b| b.to_string()).collect()
    }

    #[test]
    fn unchanged_blocks_produce_no_diff() {
        let (added, removed) = diff_blocks(&fps(&["a", "b"]), &owned(&["a", "b"]));
        assert!(added.is_empty());
        assert!(removed.is_empty());
    }

    #[test]
    fn appended_block_is_reported_with_index_len_and_preview() {
        let (added, removed) = diff_blocks(&fps(&["core"]), &owned(&["core", "新增\n内容"]));
        assert_eq!(
            added,
            vec![AddedBlock {
                index: 1,
                len: "新增\n内容".len(),
                preview: "新增\n内容".to_string(),
            }]
        );
        assert!(removed.is_empty());
    }

    #[test]
    fn changed_block_with_same_length_is_added_and_removed() {
        let (added, removed) = diff_blocks(&fps(&["x=1"]), &owned(&["x=2"]));
        assert_eq!(added.len(), 1);
        assert_eq!(removed, vec![3]);
    }

    #[test]
    fn duplicate_blocks_are_matched_by_count() {
        let (added, removed) = diff_blocks(&fps(&["a", "a"]), &owned(&["a", "a", "a"]));
        assert_eq!(added.len(), 1);
        assert_eq!(added[0].index, 2);
        assert!(removed.is_empty());
    }

    #[derive(Clone, Default)]
    struct BufWriter(std::sync::Arc<std::sync::Mutex<Vec<u8>>>);

    impl std::io::Write for BufWriter {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(buf);
            Ok(buf.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    #[test]
    fn observe_logs_added_block_only_when_key_changed_in_known_session() {
        let buf = BufWriter::default();
        let writer = buf.clone();
        let subscriber = tracing_subscriber::fmt()
            .with_max_level(tracing::Level::INFO)
            .with_ansi(false)
            .with_writer(move || writer.clone())
            .finish();
        tracing::subscriber::with_default(subscriber, || {
            let sid = "drift-test-session";
            let d = &mut DriftDiag::default();
            // 会话首次出现：没有上一次可比，不输出
            observe(d, sid, &["core prompt"], true);
            // key 未变：只更新快照，不输出
            observe(d, sid, &["core prompt"], false);
            // key 变化：输出新增块
            observe(d, sid, &["core prompt", "APPENDED-BY-CC reminder"], true);
        });
        let out = String::from_utf8(buf.0.lock().unwrap().clone()).unwrap();
        assert_eq!(out.matches("[drift]").count(), 1, "{out}");
        assert!(out.contains("APPENDED-BY-CC reminder"), "{out}");
        assert!(out.contains("#1(23B)"), "{out}");
        assert!(out.contains("added=1"), "{out}");
    }

    fn capture<F: FnOnce()>(f: F) -> String {
        capture_at(tracing::Level::INFO, f)
    }

    fn capture_at<F: FnOnce()>(level: tracing::Level, f: F) -> String {
        let buf = BufWriter::default();
        let writer = buf.clone();
        let subscriber = tracing_subscriber::fmt()
            .with_max_level(level)
            .with_ansi(false)
            .with_writer(move || writer.clone())
            .finish();
        tracing::subscriber::with_default(subscriber, f);
        String::from_utf8(buf.0.lock().unwrap().clone()).unwrap()
    }

    #[test]
    fn shared_bytes_counts_multiset_overlap() {
        let a = fps(&["aaaa", "bb", "bb"]);
        assert_eq!(shared_bytes(&a, &fps(&["bb", "aaaa"])), 6);
        assert_eq!(shared_bytes(&a, &fps(&["bb", "bb", "bb"])), 4);
        assert_eq!(shared_bytes(&a, &fps(&["zz"])), 0);
    }

    #[test]
    fn auxiliary_request_is_not_reported_as_drift_of_main_request() {
        let main_a = "M".repeat(6000);
        let main_b = "N".repeat(4000);
        let aux = "short auxiliary prompt ".repeat(50);
        let out = capture(|| {
            let sid = "drift-aux-session";
            let d = &mut DriftDiag::default();
            // 先到的辅助请求（如标题生成）：会话首次出现，无输出
            observe(d, sid, &[&aux], true);
            // 主请求首次出现：与辅助请求无重叠 → 新请求类型，而不是「新增 / 消失」
            observe(d, sid, &[&main_a, &main_b], true);
            // 辅助请求再次出现但 key 未变：只更新快照
            observe(d, sid, &[&aux], false);
            // 主请求中途真的追加了一块：必须和主请求快照比较（added=1 removed=0）
            observe(d, sid, &[&main_a, &main_b, "APPENDED-BY-CC reminder"], true);
        });
        assert_eq!(out.matches("新请求类型").count(), 1, "{out}");
        assert_eq!(out.matches("缓存 key 变化").count(), 1, "{out}");
        assert!(out.contains("added=1"), "{out}");
        assert!(out.contains("removed=0"), "{out}");
        assert!(out.contains("APPENDED-BY-CC reminder"), "{out}");
    }

    #[test]
    fn snapshots_per_session_are_capped() {
        let sid = "drift-cap-session";
        let mut d = DriftDiag::default();
        capture(|| {
            for i in 0..(MAX_SNAPSHOTS_PER_SESSION + 3) {
                observe(&mut d, sid, &[&format!("kind-{i}-").repeat(500)], true);
            }
        });
        assert_eq!(d.snapshots.len(), MAX_SNAPSHOTS_PER_SESSION);
    }

    #[test]
    fn log_diverted_lists_blocks_and_skips_empty() {
        let buf = BufWriter::default();
        let writer = buf.clone();
        let subscriber = tracing_subscriber::fmt()
            .with_max_level(tracing::Level::INFO)
            .with_ansi(false)
            .with_writer(move || writer.clone())
            .finish();
        tracing::subscriber::with_default(subscriber, || {
            let d = &mut DriftDiag::default();
            let reminder = "  The task tools haven't been used recently.\nmore";
            log_diverted(d, "s", &[]);
            log_diverted(d, "s", &[reminder]);
            // 数量未变化：不重复输出
            log_diverted(d, "s", &[reminder]);
        });
        let out = String::from_utf8(buf.0.lock().unwrap().clone()).unwrap();
        assert_eq!(out.matches("[drift]").count(), 1, "{out}");
        assert!(out.contains("diverted=1"), "{out}");
        assert!(out.contains("The task tools haven't been used"), "{out}");
        assert!(out.contains("(49B)"), "{out}");
    }

    #[test]
    fn info_logs_include_previews_without_duplicates_at_debug() {
        let out = capture_at(tracing::Level::DEBUG, || {
            let sid = "drift-debug-preview-session";
            let d = &mut DriftDiag::default();
            observe(d, sid, &["core prompt"], true);
            observe(d, sid, &["core prompt", "PRIVATE-STABLE-PREVIEW"], true);
            log_diverted(d, sid, &["PRIVATE-DYNAMIC-PREVIEW"]);
        });
        let info: Vec<&str> = out.lines().filter(|line| line.contains(" INFO ")).collect();
        let debug: Vec<&str> = out
            .lines()
            .filter(|line| line.contains(" DEBUG "))
            .collect();
        assert_eq!(info.len(), 2, "{out}");
        assert!(debug.is_empty(), "{out}");
        assert_eq!(out.matches("[drift]").count(), 2, "{out}");
        assert!(
            info.iter()
                .any(|line| line.contains("PRIVATE-STABLE-PREVIEW")),
            "{out}"
        );
        assert!(
            info.iter()
                .any(|line| line.contains("PRIVATE-DYNAMIC-PREVIEW")),
            "{out}"
        );
    }

    #[test]
    fn preview_is_truncated_on_char_boundary() {
        let long = "字".repeat(200);
        assert_eq!(preview(&long, PREVIEW_CHARS).chars().count(), PREVIEW_CHARS);
    }
}
