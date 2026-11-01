// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 会话 ID 提取/派生、compact 压缩请求检测

use sha2::{Digest, Sha256};

use crate::anthropic::types::MessagesRequest;

/// 验证字符串是否为合法 UUID 格式（xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx）
pub(super) fn is_valid_uuid(s: &str) -> bool {
    s.len() == 36
        && s.chars().filter(|c| *c == '-').count() == 4
        && s.chars().all(|c| c == '-' || c.is_ascii_hexdigit())
}

/// 将合法 UUID 字符串归一为 v4 形态（Version=4、Variant=8/9/A/B）并转为小写
///
/// `is_valid_uuid` 只校验格式不校验版本位：客户端可能发来 v1/nil 等非 v4 形态 UUID，
/// 上游严格的 UUID 解析器可能将其拒绝 (400 Bad Request)。归一仅覆盖 Version/Variant
/// 位；对已是 v4 形态的输入值不变（仅统一为小写）。会话身份只需跨轮稳定——
/// 客户端每轮重发同一原值，归一结果幂等，sticky 与上游 prompt cache 不受影响。
pub(super) fn normalize_uuid_v4(s: &str) -> String {
    let Ok(u) = uuid::Uuid::parse_str(s) else {
        return s.to_string();
    };
    let mut b = *u.as_bytes();
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    uuid::Uuid::from_bytes(b).to_string()
}

/// 从 metadata.user_id 中提取 session UUID
///
/// 支持三种格式：
/// 1. 纯 UUID: 客户端显式声明的会话身份直接采用（OpenAI user 字段透传场景）
/// 2. 标准格式: user_xxx_account__session_0b4445e1-f5be-49e1-87ce-62bbc28ad705
/// 3. JSON 格式: {"session_id":"UUID"} 或 {"id":"UUID"}（Claude Code 2.1.128+）
///
/// 所有路径返回前均归一为 v4 形态（小写），见 [`normalize_uuid_v4`]。
pub(super) fn extract_session_id(user_id: &str) -> Option<String> {
    // 纯 UUID 直通：显式声明的会话身份应优先于 fallback 派生
    if is_valid_uuid(user_id) {
        return Some(normalize_uuid_v4(user_id));
    }
    // 尝试 JSON 格式解析（Claude Code 新版本发送 JSON 字符串作为 user_id）
    if user_id.trim_start().starts_with('{')
        && let Ok(v) = serde_json::from_str::<serde_json::Value>(user_id)
    {
        for key in &["session_id", "id"] {
            if let Some(id) = v.get(key).and_then(|v| v.as_str())
                && is_valid_uuid(id)
            {
                return Some(normalize_uuid_v4(id));
            }
        }
    }
    // 标准格式: 查找 "session_" 后面的 UUID
    if let Some(pos) = user_id.find("session_") {
        let session_part = &user_id[pos + 8..]; // "session_" 长度为 8
        if let Some(uuid_str) = session_part.get(..36) {
            // 严格验证：UUID 只能包含 hex 字符和连字符，排除 JSON 污染值如 id":"xxx
            if is_valid_uuid(uuid_str) {
                return Some(normalize_uuid_v4(uuid_str));
            }
        }
    }
    None
}

/// 从 conversationId 派生稳定的 agentContinuationId
///
/// 使用 conversationId 的 SHA-256 哈希前 16 字节，格式化为 UUID 形式。
/// 同一 conversationId 始终产生相同的 agentContinuationId。
/// （会话亲和路由按 conversationId；上游 prompt cache 两者都不依赖，见 [`derive_conversation_id`]）
pub(super) fn derive_agent_continuation_id(conversation_id: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(b"agent-continuation:");
    hasher.update(conversation_id.as_bytes());
    let result = hasher.finalize();
    format!(
        "{:02x}{:02x}{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}{:02x}{:02x}{:02x}{:02x}",
        result[0],
        result[1],
        result[2],
        result[3],
        result[4],
        result[5],
        result[6],
        result[7],
        result[8],
        result[9],
        result[10],
        result[11],
        result[12],
        result[13],
        result[14],
        result[15]
    )
}

/// 按字符边界截断到最多 `max_chars` 个字符，避免超长文本拖慢 hash 计算。
pub(super) fn truncate_chars(s: &str, max_chars: usize) -> &str {
    match s.char_indices().nth(max_chars) {
        Some((idx, _)) => &s[..idx],
        None => s,
    }
}

/// 从消息 content 中提取用于哈希的文本，累计到 `max_chars` 字符即停止。
///
/// 只取顶层 text 块：`image` / `document` 的 base64 数据对区分会话没有额外价值，
/// 而直接 `Display` 整个 content 会把它们完整实体化一遍（可达数 MB），
/// 且同一会话每轮请求都会重算，代价白付。
/// 代价是「文本相同、仅附件不同」的两个会话会被判为同一会话，可接受。
pub(super) fn collect_text_for_hash(content: &serde_json::Value, max_chars: usize) -> String {
    let mut out = String::new();
    match content {
        serde_json::Value::String(s) => out.push_str(truncate_chars(s, max_chars)),
        serde_json::Value::Array(arr) => {
            let mut remaining = max_chars;
            for item in arr {
                if remaining == 0 {
                    break;
                }
                if let Some(text) = item.get("text").and_then(|v| v.as_str()) {
                    let piece = truncate_chars(text, remaining);
                    remaining -= piece.chars().count();
                    out.push_str(piece);
                }
            }
        }
        _ => {}
    }
    out
}

/// 检测当前请求是否为 Claude Code 的 `/compact`（手动触发或 auto-compact）压缩请求。
///
/// 命中任意一个信号即判定为压缩请求：
///   1. 最后一条用户消息文本以 `/compact` 开头（手动触发）。
///   2. 最后一条用户消息包含 Claude Code v2.1+ 的 reactive-compact 摘要提示词
///      特征（`"critical: respond with text only"` + `"create a detailed
///      summary of this/the conversation"`），手动 `/compact` 与 auto-compact
///      均会发送这段提示词。
///
/// 参考 agy-cc-proxy 项目 `isCompactRequest` 的生产验证信号（2026-08 生产日志
/// 证实这两个字符串稳定存在）。误判为压缩（实际不是）最坏情况只是多等一会儿，
/// 不影响正确性；误判为非压缩（实际是压缩）会让真正的压缩请求用回普通超时，
/// 因此判断阈值倾向"宁可多判命中"。
///
/// 只检查最后一条用户消息：压缩请求是当前这一轮的意图，不应该被更早的历史
/// 消息误判影响。
pub(crate) fn is_compact_request(messages: &[crate::anthropic::types::Message]) -> bool {
    let Some(last_user) = messages.iter().rev().find(|m| m.role == "user") else {
        return false;
    };
    let text = collect_text_for_hash(&last_user.content, 8192);
    if text.trim_start().starts_with("/compact") {
        return true;
    }
    let lower = text.to_ascii_lowercase();
    let has_critical = lower.contains("critical: respond with text only");
    let has_detailed = lower.contains("create a detailed summary of this conversation")
        || lower.contains("create a detailed summary of the conversation")
        || (lower.contains("create a detailed summary") && lower.contains("conversation"));
    has_critical && has_detailed
}

/// 参与会话 seed 的文本上限（字符），避免超长 system / 首条消息拖慢 hash
const SEED_TEXT_MAX_CHARS: usize = 4096;

/// 会话所属租户。conversationId 按租户派生：不同 API Key 的请求即便内容完全相同，
/// 也不会共用会话（sticky 绑定与会话级 history[0] / baseline 状态互不干扰）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Tenant(String);

impl Tenant {
    pub fn api_key(id: u32) -> Self {
        Self(format!("key:{id}"))
    }

    /// 无 API Key 上下文的请求（如管理员 Key）共用一个租户
    pub fn anonymous() -> Self {
        Self("anon".to_string())
    }
}

/// conversationId 的派生依据（日志用）
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) enum ConversationIdSource {
    /// metadata.user_id 携带的客户端会话标识 + 首条消息
    ClientSession,
    /// 无客户端会话标识：system + 工具名 + 首条消息
    Content,
}

/// 派生上游 conversationId（UUID v4 形态，上游严格的 UUID 解析器会拒绝其它形态）。
///
/// 实测（2026-11-01，`e2e/p0-identity.mjs`）上游 prompt cache 按「账号 + 内容前缀」寻址，
/// 与 conversationId 无关；conversationId 只决定 sticky 路由与会话级状态，因此要求：
/// - 同一会话跨轮稳定：客户端每轮重发完整历史，首条消息不变
/// - 不同会话不同：同一客户端会话里的主对话、子代理、辅助请求首条消息各不相同，各成一个会话
/// - 按租户隔离
///
/// seed：有客户端会话标识（metadata.user_id，能解析出 session UUID 时取 UUID，否则取原值）时
/// 为「标识 + 首条消息」；否则为「system + 排序后的工具名 + 首条消息」——system 与工具对同一
/// 客户端跨会话恒定，单独作 seed 会把所有会话折叠成同一个 ID。首条消息只取顶层文本块，
/// 文本相同、仅附件不同的两个会话会被判为同一会话。
///
/// 已知权衡：客户端压缩上下文改写了首条消息时，会话在压缩那一轮换新 ID、重新绑定账号；
/// 此时上游缓存本已因历史改写失效。
///
/// 调用方保证 `req.messages` 非空。
pub(super) fn derive_conversation_id(
    req: &MessagesRequest,
    tenant: &Tenant,
) -> (String, ConversationIdSource) {
    let client_session = req
        .metadata
        .as_ref()
        .and_then(|m| m.user_id.as_deref())
        .map(str::trim)
        .filter(|id| !id.is_empty())
        .map(|id| extract_session_id(id).unwrap_or_else(|| id.to_string()));

    let mut hasher = Sha256::new();
    hasher.update(b"conversation:v2");
    update_field(&mut hasher, tenant.0.as_bytes());
    let source = match &client_session {
        Some(session) => {
            update_field(&mut hasher, b"session");
            update_field(&mut hasher, session.as_bytes());
            ConversationIdSource::ClientSession
        }
        None => {
            update_field(&mut hasher, b"content");
            update_system_and_tools(&mut hasher, req);
            ConversationIdSource::Content
        }
    };
    let first = &req.messages[0];
    update_field(&mut hasher, first.role.as_bytes());
    update_field(
        &mut hasher,
        collect_text_for_hash(&first.content, SEED_TEXT_MAX_CHARS).as_bytes(),
    );

    (uuid_v4_from_digest(&hasher.finalize()), source)
}

/// 带长度前缀写入一个字段，避免相邻字段拼接产生歧义（"ab"+"c" 与 "a"+"bc"）
fn update_field(hasher: &mut Sha256, bytes: &[u8]) {
    hasher.update((bytes.len() as u64).to_le_bytes());
    hasher.update(bytes);
}

fn update_system_and_tools(hasher: &mut Sha256, req: &MessagesRequest) {
    let system = req
        .system
        .as_ref()
        .map(|blocks| {
            blocks
                .iter()
                .map(|b| b.text.as_str())
                .collect::<Vec<_>>()
                .join("\n")
        })
        .unwrap_or_default();
    update_field(
        hasher,
        truncate_chars(&system, SEED_TEXT_MAX_CHARS).as_bytes(),
    );

    let mut tool_names: Vec<&str> = req
        .tools
        .as_deref()
        .map(|tools| tools.iter().map(|t| t.name.as_str()).collect())
        .unwrap_or_default();
    tool_names.sort_unstable();
    update_field(hasher, &(tool_names.len() as u64).to_le_bytes());
    for name in tool_names {
        update_field(hasher, name.as_bytes());
    }
}

/// 取摘要前 16 字节并置 UUID v4 的 Version / Variant 位
fn uuid_v4_from_digest(digest: &[u8]) -> String {
    let mut bytes = [0u8; 16];
    bytes.copy_from_slice(&digest[..16]);
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    uuid::Uuid::from_bytes(bytes).to_string()
}
