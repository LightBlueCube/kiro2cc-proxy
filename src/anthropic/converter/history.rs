// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 历史消息构建：系统消息冻结、user/assistant 合并

use sha2::{Digest, Sha256};

use crate::anthropic::types::{ContentBlock, MessagesRequest};
use crate::kiro::model::requests::conversation::{
    AssistantMessage, HistoryAssistantMessage, HistoryUserMessage, Message, ReasoningContent,
    ReasoningText, UserInputMessageContext, UserMessage,
};
use crate::kiro::model::requests::tool::ToolUseEntry;

use super::message::process_message_content_with_tool_filter;
use super::prompt::{
    is_dynamic_hook_injection, is_token_budget_reminder, normalize_billing_header,
};
use super::rejected_signatures::is_rejected_signature;
use super::result::{ConversionError, HistoryReasoningReplay, HistoryThinkingLoss};
use super::session_store::SessionState;
use super::thinking::{
    generate_thinking_prefix, gpt_anti_pseudo_tag_hint, has_thinking_tags, thinking_prefix_budget,
};

/// 历史 assistant 文本回传上游前的净化：先整块剥离文本化思考（标记行 + 正文 + 时长行），
/// 再剥 dim 样式码（含模型模仿产生的字面 `[2m…[0m` 整行包裹），避免上游模型模仿该格式。
/// 顺序不可颠倒：标记行识别依赖 dim 转义包裹的兼容判断。
fn sanitize_rendered_history_text(text: &str) -> String {
    let stripped = crate::anthropic::stream::strip_rendered_thinking(text);
    crate::anthropic::stream::strip_dim_markers(&stripped).into_owned()
}

/// `build_history` 产物
pub(super) struct BuiltHistory {
    pub history: Vec<Message>,
    /// 从 history[0] 分流出的动态系统块（放到当前消息开头）
    pub dynamic_system: String,
    /// 实际注入 history[0] 的 thinking 文本标签所带预算（未注入为 None）
    pub injected_thinking_budget: Option<i32>,
    /// 带推理却没能回传上游的 assistant 轮次
    pub thinking_loss: HistoryThinkingLoss,
    /// 回传上游的推理签名
    pub reasoning_replay: HistoryReasoningReplay,
}

/// 构建历史消息
///
/// # Arguments
/// * `req` - 原始请求，用于读取 `thinking` 等配置字段
/// * `messages` - 移除内联 system 和末尾 assistant prefill 后的消息引用，末尾必为 user。
/// * `system` - 顶层 system 与内联 system 按顺序归并后的文本，不改变原请求。
/// * `model_id` - 已映射的 Kiro 模型 ID
/// * `session_id` / `session` - conversationId 及其会话状态（系统块基线、history[0] 诊断）
pub(super) fn build_history(
    req: &MessagesRequest,
    messages: &[&crate::anthropic::types::Message],
    system: Option<&[&str]>,
    model_id: &str,
    session_id: &str,
    session: &mut SessionState,
) -> Result<BuiltHistory, ConversionError> {
    let mut history = Vec::new();

    // 生成 thinking 前缀（仅 luna 不使用 Claude/Kiro 文本控制标签）
    let thinking_budget = thinking_prefix_budget(req, model_id);
    let thinking_prefix = generate_thinking_prefix(req, model_id);
    // 客户端自带标签 / 系统消息为空时前缀不注入，预算也不计
    let mut injected_thinking_budget = None;
    // GPT 系模型在客户端请求 thinking 时，额外注入反伪标签引导语（见函数文档）
    let anti_pseudo_tag_hint = gpt_anti_pseudo_tag_hint(req, model_id);

    // 1. 处理系统消息
    // 所有模型：CC 中途注入的动态块（hook 输出、工具/MCP 状态通知）逐轮累积，
    // 留在 history[0] 会使冻结缓存 key 每轮漂移、history[0] 之后的整段前缀缓存
    // 持续 miss（issue #47：claude-opus-5.5 会话 history[0] 每轮 +50~+970 字节）。
    // 这里把它们从 history[0] 拼接中剔除，并返回给调用方放到当前消息开头——模型仍可见
    // （hook 注入的用户规则不丢失），但不参与缓存 hash。
    let (stable_system, mut dynamic_system): (Vec<&str>, Vec<&str>) = match system {
        Some(blocks) => blocks
            .iter()
            .copied()
            .partition(|s| !is_dynamic_hook_injection(s) && !is_token_budget_reminder(s)),
        None => (Vec::new(), Vec::new()),
    };
    // 文本前缀只能识别已知的动态块；CC 还会中途追加按路径加载的 rules 等新块（实测一次
    // 4KB 的 rules 块使 history[0] 重置、单轮多花约 0.5 credits）。再按会话基线把
    // 「会话开始后才出现」的块也分流出去，与块的文本无关（见 `baseline` 模块）。
    let (stable_system, new_blocks) =
        super::baseline::split_new_blocks(&mut session.baselines, session_id, &stable_system);
    dynamic_system.extend(new_blocks);
    // 全部块都被分流（stable 拼接结果为空，含仅剩空文本块）时按"无系统消息"处理，
    // 保留 thinking 前缀 / 引导语注入
    let stable_content = stable_system.join("\n");
    super::drift_diag::log_diverted(&mut session.drift, session_id, &dynamic_system);
    let system_present =
        system.is_some() && (dynamic_system.is_empty() || !stable_content.is_empty());
    let dynamic_content = dynamic_system.join("\n\n");

    if system_present {
        let system_content = stable_content;

        if !system_content.is_empty() {
            // 注入thinking标签到系统消息最前面（如果需要且不存在）
            let static_content = if let Some(ref prefix) = thinking_prefix {
                if !has_thinking_tags(&system_content) {
                    injected_thinking_budget = thinking_budget;
                    format!("{}\n{}", prefix, system_content)
                } else {
                    system_content
                }
            } else {
                system_content
            };

            // 追加 GPT 反伪标签引导语（仅当请求携带 thinking 配置时）
            let final_content = if let Some(hint) = anti_pseudo_tag_hint {
                format!("{}\n{}", static_content, hint)
            } else {
                static_content
            };

            // 将 cch= 固定为 0，使 history[0] 跨请求稳定，命中 Kiro prompt cache。
            let cache_content = normalize_billing_header(final_content);

            // 诊断：同一会话里 history[0] 是否变化（变化即上游前缀缓存整段失效）。
            // 完整内容参与摘要，避免前缀相同的辅助请求混为一谈
            let digest: [u8; 32] = Sha256::digest(cache_content.as_bytes()).into();
            let mut short = [0u8; 16];
            short.copy_from_slice(&digest[..16]);
            let first = session.observe_history0(short);
            super::drift_diag::observe(&mut session.drift, session_id, &stable_system, first);
            tracing::info!(
                "[exp2] history[0] {} hash={} len={} session={}",
                if first { "first" } else { "frozen" },
                short[..8]
                    .iter()
                    .map(|b| format!("{b:02x}"))
                    .collect::<String>(),
                cache_content.len(),
                session_id
            );

            // 系统消息作为 user + assistant 配对
            let user_msg = HistoryUserMessage::new(cache_content, model_id);
            history.push(Message::User(user_msg));

            let assistant_msg = HistoryAssistantMessage::new(HistoryAssistantMessage::SYSTEM_ACK);
            history.push(Message::Assistant(assistant_msg));
        } else if let Some(hint) = anti_pseudo_tag_hint {
            // 无系统消息内容，但仍需为 GPT 系模型注入反伪标签引导语
            let user_msg = HistoryUserMessage::new(hint.to_string(), model_id);
            history.push(Message::User(user_msg));

            let assistant_msg = HistoryAssistantMessage::new(HistoryAssistantMessage::SYSTEM_ACK);
            history.push(Message::Assistant(assistant_msg));
        }
    } else if let Some(ref prefix) = thinking_prefix {
        // 没有系统消息但有thinking配置，插入新的系统消息
        injected_thinking_budget = thinking_budget;
        let user_msg = HistoryUserMessage::new(prefix.clone(), model_id);
        history.push(Message::User(user_msg));

        let assistant_msg = HistoryAssistantMessage::new(HistoryAssistantMessage::SYSTEM_ACK);
        history.push(Message::Assistant(assistant_msg));
    } else if let Some(hint) = anti_pseudo_tag_hint {
        // 没有系统消息、非 Claude thinking 协议模型（GPT 系），但客户端仍请求了
        // thinking：插入反伪标签引导语，避免模型自造 <analysis>/<summary> 等标签
        let user_msg = HistoryUserMessage::new(hint.to_string(), model_id);
        history.push(Message::User(user_msg));

        let assistant_msg = HistoryAssistantMessage::new(HistoryAssistantMessage::SYSTEM_ACK);
        history.push(Message::Assistant(assistant_msg));
    }

    // 2. 处理常规消息历史
    // 最后一条消息作为 currentMessage，不加入历史
    // 经过 prefill 预处理后，messages 末尾必定是 user，故直接截掉最后一条即可
    let history_end_index = messages.len().saturating_sub(1);

    // 收集并配对消息
    let mut user_buffer: Vec<&crate::anthropic::types::Message> = Vec::new();
    let mut assistant_buffer: Vec<&crate::anthropic::types::Message> = Vec::new();
    let mut thinking_loss = HistoryThinkingLoss::default();
    let mut reasoning_replay = HistoryReasoningReplay::default();

    for msg in &messages[..history_end_index] {
        if msg.role == "user" {
            // 先处理累积的 assistant 消息
            if !assistant_buffer.is_empty() {
                push_assistant_turn(
                    &mut history,
                    &assistant_buffer,
                    &req.model,
                    &mut thinking_loss,
                    &mut reasoning_replay,
                )?;
                assistant_buffer.clear();
            }
            user_buffer.push(msg);
        } else if msg.role == "assistant" {
            // 先处理累积的 user 消息
            if !user_buffer.is_empty() {
                let merged_user = merge_user_messages(&user_buffer, model_id)?;
                history.push(Message::User(merged_user));
                user_buffer.clear();
            }
            // 累积 assistant 消息（支持连续多条）
            assistant_buffer.push(msg);
        }
    }

    // 处理末尾累积的 assistant 消息
    if !assistant_buffer.is_empty() {
        push_assistant_turn(
            &mut history,
            &assistant_buffer,
            &req.model,
            &mut thinking_loss,
            &mut reasoning_replay,
        )?;
    }

    // 处理结尾的孤立 user 消息
    if !user_buffer.is_empty() {
        let merged_user = merge_user_messages(&user_buffer, model_id)?;
        history.push(Message::User(merged_user));

        // 自动配对一个 "OK" 的 assistant 响应
        let auto_assistant = HistoryAssistantMessage::new("OK");
        history.push(Message::Assistant(auto_assistant));
    }

    Ok(BuiltHistory {
        history,
        dynamic_system: dynamic_content,
        injected_thinking_budget,
        thinking_loss,
        reasoning_replay,
    })
}

/// 合并一轮 assistant 消息写入历史；回传的推理签名计入 `replay`，
/// 没回传推理时按其中的推理形态计入 `loss`
///
/// `model`：客户端请求的模型名，按它查拒绝缓存
fn push_assistant_turn(
    history: &mut Vec<Message>,
    turn: &[&crate::anthropic::types::Message],
    model: &str,
    loss: &mut HistoryThinkingLoss,
    replay: &mut HistoryReasoningReplay,
) -> Result<(), ConversionError> {
    let merged = merge_assistant_messages(turn, model)?;
    let replayed = merged.assistant_response_message.reasoning_content.as_ref();
    if let Some(reasoning) = replayed {
        replay.signature_chars += reasoning.reasoning_text.signature.len();
        replay.blocks += 1;
    }
    match lost_thinking_form(turn).filter(|_| replayed.is_none()) {
        Some(LostThinkingForm::Unsigned) => loss.unsigned_turns += 1,
        Some(LostThinkingForm::Rejected) => loss.invalid_turns += 1,
        Some(LostThinkingForm::Rendered) => loss.rendered_turns += 1,
        None => {}
    }
    history.push(Message::Assistant(merged));
    Ok(())
}

/// 签名是否出自上游（非空且不是代理伪造的）
fn is_genuine_signature(signature: &str) -> bool {
    !signature.is_empty() && !crate::anthropic::stream::is_fake_signature(signature)
}

/// 没能回传的推理在客户端历史里的形态
enum LostThinkingForm {
    /// thinking 块，但没有可回传签名
    Unsigned,
    /// thinking 块带上游签名，但该签名被上游拒绝过
    Rejected,
    /// thinkingAsText 渲染出的文本
    Rendered,
}

/// 一轮 assistant 消息里的推理形态；thinking 块优先（它才可能带签名）
///
/// 只在该轮没有回传任何推理时调用，所以带上游签名的 thinking 块必然是被拒绝缓存挡下的。
fn lost_thinking_form(turn: &[&crate::anthropic::types::Message]) -> Option<LostThinkingForm> {
    let is_rendered = |text: &str| {
        matches!(
            crate::anthropic::stream::strip_rendered_thinking(text),
            std::borrow::Cow::Owned(_)
        )
    };
    fn str_field<'a>(block: &'a serde_json::Value, key: &str) -> Option<&'a str> {
        block.get(key).and_then(serde_json::Value::as_str)
    }
    let blocks = || {
        turn.iter()
            .filter_map(|msg| msg.content.as_array())
            .flatten()
    };

    let mut thinking_blocks = blocks()
        .filter(|block| str_field(block, "type") == Some("thinking"))
        .peekable();
    if thinking_blocks.peek().is_some() {
        let has_genuine_signature = thinking_blocks
            .any(|block| str_field(block, "signature").is_some_and(is_genuine_signature));
        return Some(if has_genuine_signature {
            LostThinkingForm::Rejected
        } else {
            LostThinkingForm::Unsigned
        });
    }

    let rendered_string = turn
        .iter()
        .filter_map(|msg| msg.content.as_str())
        .any(is_rendered);
    let rendered_block = blocks()
        .filter(|block| str_field(block, "type") == Some("text"))
        .any(|block| str_field(block, "text").is_some_and(is_rendered));
    (rendered_string || rendered_block).then_some(LostThinkingForm::Rendered)
}

/// 合并多个 user 消息
pub(super) fn merge_user_messages(
    messages: &[&crate::anthropic::types::Message],
    model_id: &str,
) -> Result<HistoryUserMessage, ConversionError> {
    let mut content_parts = Vec::new();
    let mut all_images = Vec::new();
    let mut all_tool_results = Vec::new();

    for msg in messages {
        let (text, images, tool_results) =
            process_message_content_with_tool_filter(&msg.content, all_images.len(), |_| true)?;
        if !text.is_empty() {
            content_parts.push(text);
        }
        all_images.extend(images);
        all_tool_results.extend(tool_results);
    }

    let content = content_parts.join("\n");
    // 空 content 兜底：历史 user 消息中仅含 tool_result 时，Kiro 不接受空字符串。
    // 与 convert_request 保持同样占位词，避免 "Continue" 误导模型。
    let content = if content.is_empty() {
        "(tool result above)".to_string()
    } else {
        content
    };
    // 保留文本内容，即使有工具结果也不丢弃用户文本
    let mut user_msg = UserMessage::new(&content, model_id);

    if !all_images.is_empty() {
        user_msg = user_msg.with_images(all_images);
    }

    if !all_tool_results.is_empty() {
        let mut ctx = UserInputMessageContext::new();
        ctx = ctx.with_tool_results(all_tool_results);
        user_msg = user_msg.with_context(ctx);
    }

    Ok(HistoryUserMessage {
        user_input_message: user_msg,
    })
}

/// 转换 assistant 消息
///
/// `model`：客户端请求的模型名，被该模型拒绝过的签名不回传
pub(super) fn convert_assistant_message(
    msg: &crate::anthropic::types::Message,
    model: &str,
) -> Result<HistoryAssistantMessage, ConversionError> {
    let mut text_content = String::new();
    let mut tool_uses = Vec::new();
    let mut reasoning: Option<ReasoningContent> = None;

    match &msg.content {
        serde_json::Value::String(s) => {
            text_content = sanitize_rendered_history_text(s);
        }
        serde_json::Value::Array(arr) => {
            for item in arr {
                match serde_json::from_value::<ContentBlock>(item.clone()) {
                    Ok(block) => match block.block_type.as_str() {
                        // 带上游真实签名的 thinking 块回传为 reasoningContent（明文 + 签名配对）；
                        // 无签名、代理伪造签名、被上游拒绝过的签名不回传。
                        // 一条消息含多个块时取最后一个：它覆盖到该轮结束的推理。
                        "thinking" => {
                            if let Some(signature) = block.signature.filter(|s| {
                                is_genuine_signature(s) && !is_rejected_signature(model, s)
                            }) {
                                reasoning = Some(ReasoningContent {
                                    reasoning_text: ReasoningText {
                                        text: block.thinking.unwrap_or_default(),
                                        signature,
                                    },
                                });
                            }
                        }
                        "text" => {
                            if let Some(text) = block.text {
                                // thinkingAsText 渲染出的 text 块：与上方原生 thinking 一致整块
                                // 丢弃（标记行 + 思考正文 + 时长行），上游上下文不含思考内容；
                                // 仅在找不到时长行（流被中断）时降级为只剥标记行。
                                text_content.push_str(&sanitize_rendered_history_text(&text));
                            }
                        }
                        "tool_use" => {
                            if let (Some(id), Some(name)) = (block.id, block.name) {
                                let input = block.input.unwrap_or(serde_json::json!({}));
                                tool_uses.push(ToolUseEntry::new(id, name).with_input(input));
                            }
                        }
                        _ => {}
                    },
                    Err(e) => {
                        tracing::warn!("历史内容块反序列化失败，块被丢弃: {}", e);
                    }
                }
            }
        }
        _ => {}
    }

    // Kiro API 要求 content 字段不能为空：只有 tool_use，或文本全是被丢弃的思考块
    // （thinking / 文本化思考）而清空时，都需要占位符。
    // 注意：此处与 user 侧（convert_request / merge_user_messages）的 "(tool result above)"
    // 策略不同 —— assistant 侧是"模型自己历史的 tool_use 调用"，仅需占位无需语义引导；
    // 而 user 侧需明示"上方为工具结果"以避免模型误读为"用户让我继续"。
    let final_content = if text_content.is_empty() {
        " ".to_string()
    } else {
        text_content
    };

    // 确定性 messageId：基于 content + tool_use IDs 做 SHA-256，保证同一历史条目跨请求稳定
    let message_id = {
        let mut seed = String::from("assistant-msg:");
        seed.push_str(&final_content);
        for tu in &tool_uses {
            seed.push(':');
            seed.push_str(&tu.tool_use_id);
        }
        let hash = Sha256::digest(seed.as_bytes());
        format!(
            "{:02x}{:02x}{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}{:02x}{:02x}{:02x}{:02x}",
            hash[0],
            hash[1],
            hash[2],
            hash[3],
            hash[4],
            hash[5],
            hash[6],
            hash[7],
            hash[8],
            hash[9],
            hash[10],
            hash[11],
            hash[12],
            hash[13],
            hash[14],
            hash[15]
        )
    };

    let mut assistant = AssistantMessage {
        message_id: Some(message_id),
        content: final_content,
        tool_uses: None,
        reasoning_content: reasoning,
    };
    if !tool_uses.is_empty() {
        assistant = assistant.with_tool_uses(tool_uses);
    }

    Ok(HistoryAssistantMessage {
        assistant_response_message: assistant,
    })
}

/// 合并多个连续的 assistant 消息为一条
/// 用于处理网络不稳定时产生的连续 assistant 消息（Issue #79）
pub(super) fn merge_assistant_messages(
    messages: &[&crate::anthropic::types::Message],
    model: &str,
) -> Result<HistoryAssistantMessage, ConversionError> {
    assert!(!messages.is_empty());
    if messages.len() == 1 {
        return convert_assistant_message(messages[0], model);
    }

    let mut all_tool_uses: Vec<ToolUseEntry> = Vec::new();
    let mut content_parts: Vec<String> = Vec::new();
    let mut reasoning_content = None;

    for msg in messages {
        let converted = convert_assistant_message(msg, model)?;
        let am = converted.assistant_response_message;
        if !am.content.trim().is_empty() {
            content_parts.push(am.content);
        }
        if let Some(tus) = am.tool_uses {
            all_tool_uses.extend(tus);
        }
        // 合并后保留最新一条的推理：它覆盖到合并轮次结束
        if am.reasoning_content.is_some() {
            reasoning_content = am.reasoning_content;
        }
    }

    // 各条的占位符 " " 已被上面的 trim 过滤；全部为空（只有 tool_use，或全是被丢弃的
    // 思考块）时必须重新补占位符，与 convert_assistant_message 的非空约束保持一致
    let content = if content_parts.is_empty() {
        " ".to_string()
    } else {
        content_parts.join("\n\n")
    };

    let message_id = {
        let mut seed = String::from("assistant-msg:");
        seed.push_str(&content);
        for tu in &all_tool_uses {
            seed.push(':');
            seed.push_str(&tu.tool_use_id);
        }
        let hash = Sha256::digest(seed.as_bytes());
        format!(
            "{:02x}{:02x}{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}{:02x}{:02x}{:02x}{:02x}",
            hash[0],
            hash[1],
            hash[2],
            hash[3],
            hash[4],
            hash[5],
            hash[6],
            hash[7],
            hash[8],
            hash[9],
            hash[10],
            hash[11],
            hash[12],
            hash[13],
            hash[14],
            hash[15]
        )
    };

    let mut assistant = AssistantMessage {
        message_id: Some(message_id),
        content,
        tool_uses: None,
        reasoning_content,
    };
    if !all_tool_uses.is_empty() {
        assistant = assistant.with_tool_uses(all_tool_uses);
    }
    Ok(HistoryAssistantMessage {
        assistant_response_message: assistant,
    })
}
