// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 工具定义转换与 tool_use/tool_result 配对校验

use crate::kiro::model::requests::conversation::Message;
use crate::kiro::model::requests::tool::{InputSchema, Tool, ToolResult, ToolSpecification};

use super::schema::normalize_json_schema;

/// 验证并过滤 tool_use/tool_result 配对
///
/// 收集所有 tool_use_id，验证 tool_result 是否匹配
/// 静默跳过孤立的 tool_use 和 tool_result，输出警告日志
///
/// # Arguments
/// * `history` - 历史消息引用
/// * `tool_results` - 当前消息中的 tool_result 列表
///
/// # Returns
/// 元组：(经过验证和过滤后的 tool_result 列表, 孤立的 tool_use_id 集合)
pub(super) fn validate_tool_pairing(
    history: &[Message],
    tool_results: &[ToolResult],
) -> (Vec<ToolResult>, std::collections::HashSet<String>) {
    use std::collections::HashSet;

    // 1. 收集所有历史中的 tool_use_id
    let mut all_tool_use_ids: HashSet<String> = HashSet::new();
    // 2. 收集历史中已经有 tool_result 的 tool_use_id
    let mut history_tool_result_ids: HashSet<String> = HashSet::new();

    for msg in history {
        match msg {
            Message::Assistant(assistant_msg) => {
                if let Some(ref tool_uses) = assistant_msg.assistant_response_message.tool_uses {
                    for tool_use in tool_uses {
                        all_tool_use_ids.insert(tool_use.tool_use_id.clone());
                    }
                }
            }
            Message::User(user_msg) => {
                // 收集历史 user 消息中的 tool_results
                for result in &user_msg
                    .user_input_message
                    .user_input_message_context
                    .tool_results
                {
                    history_tool_result_ids.insert(result.tool_use_id.clone());
                }
            }
        }
    }

    // 3. 计算真正未配对的 tool_use_ids（排除历史中已配对的）
    let mut unpaired_tool_use_ids: HashSet<String> = all_tool_use_ids
        .difference(&history_tool_result_ids)
        .cloned()
        .collect();

    // 4. 过滤并验证当前消息的 tool_results
    let mut filtered_results = Vec::new();

    for result in tool_results {
        if unpaired_tool_use_ids.contains(&result.tool_use_id) {
            // 配对成功
            filtered_results.push(result.clone());
            unpaired_tool_use_ids.remove(&result.tool_use_id);
        } else if all_tool_use_ids.contains(&result.tool_use_id) {
            // tool_use 存在但已经在历史中配对过了，这是重复的 tool_result
            tracing::warn!(
                "跳过重复的 tool_result：该 tool_use 已在历史中配对，tool_use_id={}",
                result.tool_use_id
            );
        } else {
            // 孤立 tool_result - 找不到对应的 tool_use
            tracing::warn!(
                "跳过孤立的 tool_result：找不到对应的 tool_use，tool_use_id={}",
                result.tool_use_id
            );
        }
    }

    // 5. 检测真正孤立的 tool_use（有 tool_use 但在历史和当前消息中都没有 tool_result）
    for orphaned_id in &unpaired_tool_use_ids {
        tracing::warn!(
            "检测到孤立的 tool_use：找不到对应的 tool_result，将从历史中移除，tool_use_id={}",
            orphaned_id
        );
    }

    (filtered_results, unpaired_tool_use_ids)
}

/// 从历史消息中移除孤立的 tool_use
///
/// Kiro API 要求每个 tool_use 必须有对应的 tool_result，否则返回 400 Bad Request。
/// 此函数遍历历史中的 assistant 消息，移除没有对应 tool_result 的 tool_use。
///
/// # Arguments
/// * `history` - 可变的历史消息列表
/// * `orphaned_ids` - 需要移除的孤立 tool_use_id 集合
pub(super) fn remove_orphaned_tool_uses(
    history: &mut [Message],
    orphaned_ids: &std::collections::HashSet<String>,
) {
    if orphaned_ids.is_empty() {
        return;
    }

    for msg in history.iter_mut() {
        if let Message::Assistant(assistant_msg) = msg
            && let Some(ref mut tool_uses) = assistant_msg.assistant_response_message.tool_uses
        {
            let original_len = tool_uses.len();
            tool_uses.retain(|tu| !orphaned_ids.contains(&tu.tool_use_id));

            // 如果移除后为空，设置为 None
            if tool_uses.is_empty() {
                assistant_msg.assistant_response_message.tool_uses = None;
            } else if tool_uses.len() != original_len {
                tracing::debug!(
                    "从 assistant 消息中移除了 {} 个孤立的 tool_use",
                    original_len - tool_uses.len()
                );
            }
        }
    }
}

/// 分块写入约束的默认行数上限：0，即不追加约束。
///
/// 该约束只有旧 Amazon Q API（`upstreamApi = generateAssistantResponse`）需要：它对大块工具
/// 参数静默约 240s 后重置流（issue #46）。默认的 createResponse 没有这个时间限制（用户实测
/// 一次写入一千多行文件不再被重置），追加约束只会让模型把大写入拆成多轮、多花 credits。
///
/// 使用旧 API 时建议设为 300，依据（issue #46 实测）：单次写入约 8000 汉字耗时约 215s 仍可
/// 成功，约 20000 汉字约 240s 必定被重置，折算输出速度约 40 token/s。代码按每行约 10~15
/// token 估算，300 行约 3000~4500 token，生成约需 75~115s，距 240s 仍有 2 倍以上余量；
/// 原先的 150 行余量过大，会把本可一次写完的内容拆成多轮，每轮都重新思考，credits 随之上升
/// （issue #46 后续评论的实测数据：约 ×3）。该估算没有在真实 Claude Code 上验证；中文密集
/// 内容每行 token 更多，如仍遇到 Reset，可调小 `writeChunkingMaxLines`。
pub const DEFAULT_WRITE_CHUNKING_MAX_LINES: u32 = 0;

/// 当前生效的分块写入行数上限，0 表示不追加约束。main.rs 启动时按 config 初始化。
static WRITE_CHUNKING_MAX_LINES: std::sync::atomic::AtomicU32 =
    std::sync::atomic::AtomicU32::new(DEFAULT_WRITE_CHUNKING_MAX_LINES);

/// 设置分块写入行数上限（0 = 关闭约束）。
pub fn set_write_chunking_max_lines(max_lines: u32) {
    WRITE_CHUNKING_MAX_LINES.store(max_lines, std::sync::atomic::Ordering::Relaxed);
}

/// 读取当前分块写入行数上限。
pub fn write_chunking_max_lines() -> u32 {
    WRITE_CHUNKING_MAX_LINES.load(std::sync::atomic::Ordering::Relaxed)
}

/// 请求带 Write / Edit / MultiEdit 任一工具时追加到系统提示末尾的分块写入约束。
///
/// 背景（issue #46）：上游不会增量流式输出工具参数，模型生成大块 `tool_use` 参数期间上游
/// 对该流不发送任何数据，静默约 240s 后以 `RST_STREAM(INTERNAL_ERROR)` 重置；原样重试会
/// 再次失败，HTTP/2 PING 保活无效。代理无法让上游提前吐数据，只能引导模型把大文件拆成多次
/// 较小的调用。
///
/// 行数上限覆盖所有会携带大块文本参数的编辑类工具：Write 的 `content`、Edit 的
/// `new_string`、MultiEdit 单次调用内所有编辑的合计。MultiEdit 把多处修改合并进一次调用，
/// 参数总量按合计计算，否则逐处都小于上限也会整体超时。
///
/// 3.4.0 起移除了原先的 Write/Edit 分块策略（工具描述后缀 + 系统提示），这里只生成系统提示、
/// 不改动客户端的工具描述（见 `test_client_workflow_tool_descriptions_are_not_augmented`）。
/// 同一 `max_lines` 的输出是确定的，不影响 prompt cache 的前缀稳定性。
pub(super) fn write_chunking_system_note(max_lines: u32) -> String {
    format!(
        "IMPORTANT: The service aborts any single tool call whose arguments take longer than about 4 minutes to generate. Never put more than about {max_lines} lines of content in the arguments of one tool call. This limit applies to the Write content, to the Edit new_string, and to the combined size of all edits inside one MultiEdit call. For anything larger, write the first part with Write (or one Edit) and then append each further part with separate Edit or Bash calls, splitting a large MultiEdit into several smaller calls. Follow this silently without mentioning this limit."
    )
}

/// 工具列表中是否包含会携带大块文本参数的编辑类工具（Write / Edit / MultiEdit，名称不区分大小写）
pub(super) fn has_content_writing_tool(tools: &[Tool]) -> bool {
    tools.iter().any(|t| {
        let name = t.tool_specification.name.as_str();
        ["write", "edit", "multiedit"]
            .iter()
            .any(|n| name.eq_ignore_ascii_case(n))
    })
}

/// 是否为 Anthropic 服务端 advisor 工具（`type` 形如 `advisor_20260301`）。
/// 只认 type，不按名称匹配，避免误伤用户自己定义的同名普通工具。
fn is_advisor_server_tool(t: &crate::anthropic::types::Tool) -> bool {
    t.tool_type
        .as_deref()
        .is_some_and(|ty| ty.starts_with("advisor_"))
}

/// 转换工具定义
pub(super) fn convert_tools(tools: &Option<Vec<crate::anthropic::types::Tool>>) -> Vec<Tool> {
    let Some(tools) = tools else {
        return Vec::new();
    };

    let mut converted = Vec::new();
    let mut seen = std::collections::HashSet::new();

    for t in tools {
        // Anthropic 服务端 advisor 工具（advisor_YYYYMMDD）由 Anthropic 侧执行，Kiro 无此能力；
        // 作为普通工具发给 Kiro 会让模型发起客户端无法执行的 `advisor` 调用，所以直接剔除
        if is_advisor_server_tool(t) {
            tracing::info!(tool_type = ?t.tool_type, "跳过 advisor 服务端工具（Kiro 不支持）");
            continue;
        }

        let name = t.name.trim();
        if name.is_empty() {
            tracing::warn!("跳过空名称工具定义");
            continue;
        }

        let name_key = name.to_lowercase();
        if !seen.insert(name_key) {
            tracing::warn!(tool_name = name, "跳过重复工具定义");
            continue;
        }

        let mut description = t.description.trim().to_string();
        if description.is_empty() {
            description = format!("Tool available to the assistant: {}", name);
        }

        // 限制描述长度为 10000 字符（安全截断 UTF-8，单次遍历）
        let description = match description.char_indices().nth(10000) {
            Some((idx, _)) => description[..idx].to_string(),
            None => description,
        };

        converted.push(Tool {
            tool_specification: ToolSpecification {
                name: name.to_string(),
                description,
                input_schema: InputSchema::from_json(normalize_json_schema(serde_json::json!(
                    t.input_schema
                ))),
            },
        });
    }

    converted
}
