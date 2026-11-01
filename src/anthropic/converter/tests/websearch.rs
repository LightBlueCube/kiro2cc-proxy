//! converter 测试（自 tests.rs 拆分，纯代码搬移）
#![cfg(test)]

use super::super::websearch::{
    collect_history_tool_names, is_web_search_server_tool, strip_web_search_server_tool,
};
use super::convert_request;
use super::fallback_req;
use crate::anthropic::types::MessagesRequest;
use crate::kiro::model::requests::conversation::Message;
use crate::kiro::model::requests::conversation::{AssistantMessage, HistoryAssistantMessage};

fn ws_tool(
    tool_type: Option<&str>,
    name: &str,
    max_uses: Option<i32>,
) -> crate::anthropic::types::Tool {
    crate::anthropic::types::Tool {
        tool_type: tool_type.map(|s| s.to_string()),
        name: name.to_string(),
        description: String::new(),
        input_schema: Default::default(),
        max_uses,
        defer_loading: None,
    }
}

/// 客户端自己实现的普通 web_search 工具（带 input_schema，由客户端执行）
fn client_web_search_tool() -> crate::anthropic::types::Tool {
    crate::anthropic::types::Tool {
        tool_type: None,
        name: "web_search".to_string(),
        description: "Search the web".to_string(),
        input_schema: serde_json::from_value(serde_json::json!({
            "type": "object",
            "properties": {"query": {"type": "string"}},
            "required": ["query"]
        }))
        .unwrap(),
        max_uses: None,
        defer_loading: None,
    }
}

fn context_tool_names(req: &MessagesRequest) -> Vec<String> {
    convert_request(req)
        .unwrap()
        .conversation_state
        .current_message
        .user_input_message
        .user_input_message_context
        .tools
        .iter()
        .map(|t| t.tool_specification.name.clone())
        .collect()
}

#[test]
fn test_is_web_search_server_tool() {
    // 服务端工具按 type 前缀识别（官方格式，含未来日期版本）
    assert!(is_web_search_server_tool(&ws_tool(
        Some("web_search_20250305"),
        "web_search",
        None
    )));
    assert!(is_web_search_server_tool(&ws_tool(
        Some("web_search_20260209"),
        "web_search",
        None
    )));
    // 同名普通工具（无 type / custom）由客户端执行，不是服务端工具
    assert!(!is_web_search_server_tool(&client_web_search_tool()));
    assert!(!is_web_search_server_tool(&ws_tool(
        Some("custom"),
        "web_search",
        None
    )));
    // 其他服务端工具与普通工具不命中
    assert!(!is_web_search_server_tool(&ws_tool(
        Some("web_fetch_20250910"),
        "web_fetch",
        None
    )));
    assert!(!is_web_search_server_tool(&ws_tool(None, "Bash", None)));
}

#[test]
fn test_strip_web_search_server_tool_keeps_ordinary_tools() {
    // 混合列表：只剔除服务端工具，同名普通工具与其他工具保留
    let mut req = fallback_req(None, &["Read"], &[("user", "hi")]);
    let tools = req.tools.as_mut().unwrap();
    tools.push(ws_tool(Some("web_search_20250305"), "web_search", Some(3)));
    tools.push(client_web_search_tool());

    let stripped = strip_web_search_server_tool(&req).expect("应剔除服务端工具");
    assert_eq!(stripped.len(), 2);
    assert!(stripped.iter().any(|t| t.name == "Read"));
    assert!(
        stripped
            .iter()
            .any(|t| t.name == "web_search" && t.tool_type.is_none())
    );
}

#[test]
fn test_strip_web_search_server_tool_no_hit() {
    // 没有服务端工具（含只有同名普通工具）：返回 None，调用方借用原列表
    let mut req = fallback_req(None, &["Read", "Bash"], &[("user", "hi")]);
    assert!(strip_web_search_server_tool(&req).is_none());

    req.tools.as_mut().unwrap().push(client_web_search_tool());
    assert!(strip_web_search_server_tool(&req).is_none());

    let req = fallback_req(None, &[], &[("user", "hi")]);
    assert!(strip_web_search_server_tool(&req).is_none());
}

#[test]
fn test_convert_request_drops_server_web_search_without_injection() {
    // 服务端工具被剔除，代理不注入任何替代工具
    let mut req = fallback_req(None, &["Read"], &[("user", "搜索一下今天的新闻")]);
    req.tools
        .as_mut()
        .unwrap()
        .push(ws_tool(Some("web_search_20250305"), "web_search", Some(3)));

    assert_eq!(context_tool_names(&req), vec!["Read".to_string()]);
}

#[test]
fn test_convert_request_passes_through_client_web_search_tool() {
    // 客户端同名普通工具原样透传给上游
    let mut req = fallback_req(None, &["Read"], &[("user", "搜索一下今天的新闻")]);
    req.tools.as_mut().unwrap().push(client_web_search_tool());

    let names = context_tool_names(&req);
    assert!(names.contains(&"Read".to_string()));
    assert!(names.contains(&"web_search".to_string()));
}

#[test]
fn test_collect_history_tool_names_includes_web_search() {
    use crate::kiro::model::requests::tool::ToolUseEntry;

    // 历史里的 web_search 调用来自客户端普通工具，同样需要占位定义
    let assistant_msg = AssistantMessage::new("Let me search.").with_tool_uses(vec![
        ToolUseEntry::new("ws-1", "web_search")
            .with_input(serde_json::json!({"query": "rust news"})),
        ToolUseEntry::new("tool-1", "read").with_input(serde_json::json!({"path": "/test.txt"})),
    ]);

    let history = vec![Message::Assistant(HistoryAssistantMessage {
        assistant_response_message: assistant_msg,
    })];

    let tool_names = collect_history_tool_names(&history);
    assert!(tool_names.contains(&"web_search".to_string()));
    assert!(tool_names.contains(&"read".to_string()));
}
