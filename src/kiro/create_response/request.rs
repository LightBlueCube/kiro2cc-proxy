// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! GenerateAssistantResponse 请求体 → CreateResponse 请求体
//!
//! 与 Kiro 客户端（KAS 3.1.0 `buildCreateResponseRequest`）同构：由 `conversationState`
//! 逐条展开成 Responses 的 `input`，不使用 `previous_response_id`，每次发送完整历史。
//! converter 产出的 history（含冻结的 history[0] 系统提示配对）逐条原样映射，
//! 前缀稳定性因此与 GenerateAssistantResponse 一致。
//!
//! ```text
//! history[i] / currentMessage            input
//! ─────────────────────────────          ─────────────────────────────────────────────
//! userInputMessage                   →   function_call_output × toolResults
//!                                        {role:user, content: 文本 | [input_text, input_image…]}
//! assistantResponseMessage           →   {type:reasoning, encrypted_content}（有推理时）
//!                                        {role:assistant, content}（有正文时）
//!                                        function_call × toolUses
//! ```

use serde::Serialize;

use crate::kiro::model::requests::conversation::{AssistantMessage, KiroImage, Message};
use crate::kiro::model::requests::kiro::KiroRequest;
use crate::kiro::model::requests::tool::{Tool, ToolResult};

/// converter 未写 origin 时的取值（与 converter 一致）
const DEFAULT_ORIGIN: &str = "AI_EDITOR";

/// spec 模式的 agent mode / agentTaskType 取值；仅此模式需要在 metadata 中声明任务类型
const SPEC_TASK_AGENT_MODE: &str = "spectask";

/// 翻译结果：请求体，以及 `x-amzn-kiro-origin` / `x-amzn-kiro-profile` 头的取值
pub struct CreateResponseRequest {
    pub body: String,
    pub origin: String,
    pub profile_arn: Option<String>,
}

/// 把发往 GenerateAssistantResponse 的请求体翻译为 CreateResponse 请求体
///
/// `agent_mode` 与 GenerateAssistantResponse 的 `x-amzn-kiro-agent-mode` 头取值一致。
pub fn to_create_response_request(
    kiro_body: &str,
    agent_mode: &str,
) -> anyhow::Result<CreateResponseRequest> {
    let request: KiroRequest = serde_json::from_str(kiro_body)
        .map_err(|e| anyhow::anyhow!("CreateResponse 请求翻译失败，请求体不是 KiroRequest: {e}"))?;
    let state = &request.conversation_state;
    let current = &state.current_message.user_input_message;
    let origin = current.origin.as_deref().unwrap_or(DEFAULT_ORIGIN);

    let mut input = Vec::new();
    for message in &state.history {
        match message {
            Message::User(user) => {
                let user = &user.user_input_message;
                push_user_items(
                    &mut input,
                    &user.user_input_message_context.tool_results,
                    &user.content,
                    &user.images,
                );
            }
            Message::Assistant(assistant) => {
                push_assistant_items(&mut input, &assistant.assistant_response_message)?;
            }
        }
    }
    push_user_items(
        &mut input,
        &current.user_input_message_context.tool_results,
        &current.content,
        &current.images,
    );

    let tools = &current.user_input_message_context.tools;
    let body = RequestBody {
        model: &current.model_id,
        input,
        tools: (!tools.is_empty()).then(|| tools.iter().map(FunctionTool::from).collect()),
        stream: true,
        origin,
        profile_arn: request.profile_arn.as_deref(),
        metadata: Metadata {
            agent_mode,
            agent_task_type: (agent_mode == SPEC_TASK_AGENT_MODE).then_some(SPEC_TASK_AGENT_MODE),
            conversation_id: &state.conversation_id,
            agent_continuation_id: state.agent_continuation_id.as_deref(),
        },
        reasoning: request
            .additional_model_request_fields
            .as_ref()
            .and_then(Reasoning::from_fields),
    };

    Ok(CreateResponseRequest {
        body: serde_json::to_string(&body)?,
        origin: origin.to_string(),
        profile_arn: request.profile_arn.clone(),
    })
}

/// 工具结果先于用户正文（与 KAS 一致：tool_result 回应的是上一条 assistant 的 function_call）
fn push_user_items<'a>(
    input: &mut Vec<InputItem<'a>>,
    tool_results: &'a [ToolResult],
    content: &'a str,
    images: &'a [KiroImage],
) {
    for result in tool_results {
        input.push(InputItem::Typed(TypedItem::FunctionCallOutput {
            call_id: &result.tool_use_id,
            output: tool_result_text(result),
        }));
    }

    if images.is_empty() {
        if !content.is_empty() {
            input.push(InputItem::Message {
                role: "user",
                content: MessageContent::Text(content),
            });
        }
        return;
    }

    let mut parts = Vec::with_capacity(images.len() + 1);
    if !content.is_empty() {
        parts.push(ContentPart::InputText { text: content });
    }
    parts.extend(images.iter().map(|image| ContentPart::InputImage {
        image_url: format!(
            "data:image/{};base64,{}",
            image.format.to_lowercase(),
            image.source.bytes
        ),
    }));
    input.push(InputItem::Message {
        role: "user",
        content: MessageContent::Parts(parts),
    });
}

/// 推理、正文、工具调用依次展开
fn push_assistant_items<'a>(
    input: &mut Vec<InputItem<'a>>,
    message: &'a AssistantMessage,
) -> anyhow::Result<()> {
    if let Some(reasoning) = &message.reasoning_content {
        // 加密推理信封即 GenerateAssistantResponse 的 reasoningContent 原样 JSON
        input.push(InputItem::Typed(TypedItem::Reasoning {
            summary: [],
            encrypted_content: serde_json::to_string(reasoning)?,
        }));
    }

    if !message.content.is_empty() {
        input.push(InputItem::Message {
            role: "assistant",
            content: MessageContent::Text(&message.content),
        });
    }

    for tool_use in message.tool_uses.iter().flatten() {
        let arguments = match &tool_use.input {
            serde_json::Value::String(raw) => raw.clone(),
            other => other.to_string(),
        };
        input.push(InputItem::Typed(TypedItem::FunctionCall {
            call_id: &tool_use.tool_use_id,
            name: &tool_use.name,
            arguments,
        }));
    }
    Ok(())
}

/// 工具结果各内容块的 text 按行拼接（与 KAS 一致；图片已由 converter 移入同条消息的 images）
fn tool_result_text(result: &ToolResult) -> String {
    result
        .content
        .iter()
        .map(|block| {
            block
                .get("text")
                .and_then(serde_json::Value::as_str)
                .unwrap_or_default()
        })
        .collect::<Vec<_>>()
        .join("\n")
}

#[derive(Serialize)]
struct RequestBody<'a> {
    model: &'a str,
    input: Vec<InputItem<'a>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    tools: Option<Vec<FunctionTool<'a>>>,
    stream: bool,
    origin: &'a str,
    #[serde(rename = "profileArn", skip_serializing_if = "Option::is_none")]
    profile_arn: Option<&'a str>,
    metadata: Metadata<'a>,
    #[serde(skip_serializing_if = "Option::is_none")]
    reasoning: Option<Reasoning>,
}

/// 消息条目不带 type（与 KAS 一致），其余条目按 type 区分
#[derive(Serialize)]
#[serde(untagged)]
enum InputItem<'a> {
    Message {
        role: &'static str,
        content: MessageContent<'a>,
    },
    Typed(TypedItem<'a>),
}

#[derive(Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum TypedItem<'a> {
    FunctionCallOutput {
        call_id: &'a str,
        output: String,
    },
    FunctionCall {
        call_id: &'a str,
        name: &'a str,
        arguments: String,
    },
    Reasoning {
        summary: [(); 0],
        encrypted_content: String,
    },
}

#[derive(Serialize)]
#[serde(untagged)]
enum MessageContent<'a> {
    Text(&'a str),
    Parts(Vec<ContentPart<'a>>),
}

#[derive(Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum ContentPart<'a> {
    InputText { text: &'a str },
    InputImage { image_url: String },
}

#[derive(Serialize)]
struct FunctionTool<'a> {
    #[serde(rename = "type")]
    kind: &'static str,
    name: &'a str,
    description: &'a str,
    parameters: &'a serde_json::Value,
}

impl<'a> From<&'a Tool> for FunctionTool<'a> {
    fn from(tool: &'a Tool) -> Self {
        let spec = &tool.tool_specification;
        Self {
            kind: "function",
            name: &spec.name,
            description: &spec.description,
            parameters: &spec.input_schema.json,
        }
    }
}

#[derive(Serialize)]
struct Metadata<'a> {
    #[serde(rename = "kiro:agent_mode")]
    agent_mode: &'a str,
    #[serde(
        rename = "kiro:agent_task_type",
        skip_serializing_if = "Option::is_none"
    )]
    agent_task_type: Option<&'static str>,
    #[serde(rename = "kiro:conversation_id")]
    conversation_id: &'a str,
    #[serde(
        rename = "kiro:agent_continuation_id",
        skip_serializing_if = "Option::is_none"
    )]
    agent_continuation_id: Option<&'a str>,
}

/// additionalModelRequestFields → `reasoning`（KAS `buildReasoningField`）
///
/// 只携带 thinking.type、effort（output_config 优先于 reasoning）、thinking.display；
/// max_tokens、budget_tokens 等其余字段 CreateResponse 不收，与 KAS 一样不发。
#[derive(Serialize)]
struct Reasoning {
    #[serde(rename = "type", skip_serializing_if = "Option::is_none")]
    kind: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    effort: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    summary: Option<String>,
}

impl Reasoning {
    fn from_fields(fields: &serde_json::Value) -> Option<Self> {
        let non_empty = |pointer: &str| {
            fields
                .pointer(pointer)
                .and_then(serde_json::Value::as_str)
                .filter(|s| !s.is_empty())
                .map(str::to_string)
        };
        let reasoning = Self {
            kind: non_empty("/thinking/type"),
            effort: non_empty("/output_config/effort").or_else(|| non_empty("/reasoning/effort")),
            summary: non_empty("/thinking/display"),
        };
        let is_empty =
            reasoning.kind.is_none() && reasoning.effort.is_none() && reasoning.summary.is_none();
        (!is_empty).then_some(reasoning)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{Value, json};

    fn translate(kiro: Value) -> Value {
        let request = to_create_response_request(&kiro.to_string(), "vibe").unwrap();
        serde_json::from_str(&request.body).unwrap()
    }

    fn base_request() -> Value {
        json!({
            "conversationState": {
                "conversationId": "conv-1",
                "agentContinuationId": "cont-1",
                "agentTaskType": "vibe",
                "history": [
                    {"userInputMessage": {"content": "SYSTEM", "modelId": "claude-opus-4.6", "origin": "AI_EDITOR"}},
                    {"assistantResponseMessage": {"content": "I will follow these instructions."}},
                    {"userInputMessage": {"content": "read a.txt", "modelId": "claude-opus-4.6",
                        "images": [{"format": "PNG", "source": {"bytes": "aGk="}}]}},
                    {"assistantResponseMessage": {
                        "content": "",
                        "reasoningContent": {"reasoningText": {"text": "think", "signature": "sig"}},
                        "toolUses": [{"toolUseId": "tu-1", "name": "Read", "input": {"path": "a.txt"}}]}}
                ],
                "currentMessage": {"userInputMessage": {
                    "content": "",
                    "modelId": "claude-opus-4.6",
                    "origin": "AI_EDITOR",
                    "userInputMessageContext": {
                        "toolResults": [{"toolUseId": "tu-1", "content": [{"text": "line1"}, {"text": "line2"}], "status": "success"}],
                        "tools": [{"toolSpecification": {"name": "Read", "description": "read file",
                            "inputSchema": {"json": {"type": "object"}}}}]
                    }
                }}
            },
            "profileArn": "arn:aws:codewhisperer:us-east-1:1:profile/X",
            "additionalModelRequestFields": {
                "thinking": {"type": "adaptive", "display": "summarized"},
                "output_config": {"effort": "high"},
                "max_tokens": 32000
            }
        })
    }

    #[test]
    fn conversation_state_is_expanded_like_kiro_client() {
        let body = translate(base_request());

        assert_eq!(
            body["input"],
            json!([
                {"role": "user", "content": "SYSTEM"},
                {"role": "assistant", "content": "I will follow these instructions."},
                {"role": "user", "content": [
                    {"type": "input_text", "text": "read a.txt"},
                    {"type": "input_image", "image_url": "data:image/png;base64,aGk="}
                ]},
                {"type": "reasoning", "summary": [],
                    "encrypted_content": r#"{"reasoningText":{"text":"think","signature":"sig"}}"#},
                {"type": "function_call", "call_id": "tu-1", "name": "Read", "arguments": r#"{"path":"a.txt"}"#},
                {"type": "function_call_output", "call_id": "tu-1", "output": "line1\nline2"}
            ])
        );
    }

    #[test]
    fn top_level_fields_follow_kiro_client() {
        let body = translate(base_request());

        assert_eq!(body["model"], "claude-opus-4.6");
        assert_eq!(body["stream"], true);
        assert_eq!(body["origin"], "AI_EDITOR");
        assert_eq!(
            body["profileArn"],
            "arn:aws:codewhisperer:us-east-1:1:profile/X"
        );
        assert_eq!(
            body["tools"],
            json!([{"type": "function", "name": "Read", "description": "read file",
                "parameters": {"type": "object"}}])
        );
        assert_eq!(
            body["metadata"],
            json!({"kiro:agent_mode": "vibe", "kiro:conversation_id": "conv-1",
                "kiro:agent_continuation_id": "cont-1"})
        );
        // max_tokens 不进入 CreateResponse
        assert_eq!(
            body["reasoning"],
            json!({"type": "adaptive", "effort": "high", "summary": "summarized"})
        );
        assert!(body.get("instructions").is_none());
        assert!(body.get("previous_response_id").is_none());
    }

    #[test]
    fn optional_fields_are_omitted() {
        let mut kiro = base_request();
        kiro.as_object_mut()
            .unwrap()
            .remove("additionalModelRequestFields");
        kiro.as_object_mut().unwrap().remove("profileArn");
        kiro["conversationState"]["currentMessage"]["userInputMessage"]["userInputMessageContext"] =
            json!({});
        let body = translate(kiro);

        assert!(body.get("reasoning").is_none());
        assert!(body.get("profileArn").is_none());
        assert!(body.get("tools").is_none());
        // 空正文的当前消息不产生 user 条目
        assert_eq!(
            body["input"].as_array().unwrap().last().unwrap()["type"],
            "function_call"
        );
    }

    #[test]
    fn reasoning_effort_falls_back_to_reasoning_field() {
        let mut kiro = base_request();
        kiro["additionalModelRequestFields"] = json!({"reasoning": {"effort": "medium"}});
        assert_eq!(translate(kiro)["reasoning"], json!({"effort": "medium"}));

        let mut kiro = base_request();
        kiro["additionalModelRequestFields"] = json!({"max_tokens": 1024});
        assert!(translate(kiro).get("reasoning").is_none());
    }

    #[test]
    fn spec_task_declares_agent_task_type() {
        let request = to_create_response_request(&base_request().to_string(), "spectask").unwrap();
        let body: Value = serde_json::from_str(&request.body).unwrap();
        assert_eq!(body["metadata"]["kiro:agent_mode"], "spectask");
        assert_eq!(body["metadata"]["kiro:agent_task_type"], "spectask");
        assert_eq!(request.origin, "AI_EDITOR");
        assert_eq!(
            request.profile_arn.as_deref(),
            Some("arn:aws:codewhisperer:us-east-1:1:profile/X")
        );
    }

    #[test]
    fn string_tool_input_is_passed_through() {
        let mut kiro = base_request();
        kiro["conversationState"]["history"][3]["assistantResponseMessage"]["toolUses"][0]["input"] =
            json!("{\"raw\":1}");
        assert_eq!(translate(kiro)["input"][4]["arguments"], "{\"raw\":1}");
    }

    #[test]
    fn translation_is_deterministic() {
        // history 冻结依赖逐字节稳定：同一请求体两次翻译结果相同
        let kiro = base_request().to_string();
        let a = to_create_response_request(&kiro, "vibe").unwrap().body;
        let b = to_create_response_request(&kiro, "vibe").unwrap().body;
        assert_eq!(a, b);
    }

    #[test]
    fn invalid_body_is_an_error() {
        assert!(to_create_response_request("{}", "vibe").is_err());
    }
}
