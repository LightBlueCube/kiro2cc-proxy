// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 转换结果与错误类型：ConversionResult 聚合转换产物，ConversionError 描述 Anthropic → Kiro 转换失败场景。

use crate::kiro::model::requests::conversation::ConversationState;

/// 转换结果
#[derive(Debug)]
pub struct ConversionResult {
    /// 转换后的 Kiro 请求
    pub conversation_state: ConversationState,
    /// 模型专属请求参数（thinking、output_config、max_tokens）
    pub additional_model_request_fields: Option<serde_json::Value>,
    /// 是否为 Claude Code 的 `/compact`（手动或 auto-compact）压缩请求
    ///
    /// 用于 provider 层选择更长的上游超时（压缩请求通常处理超长历史，
    /// 耗时明显高于普通请求）。检测逻辑见 `is_compact_request`。
    pub is_compact_request: bool,
    /// 本次请求的缓存前缀（`history[0]` + 历史对话轮次）是否已命中会话级冻结缓存。
    ///
    /// 仅当存在历史对话轮次（去除内联 system 与末尾 prefill 后 `messages.len() > 1`）
    /// 且 `history[0]` 命中 `PREV_H0`（无系统消息时仅要求存在历史轮次）时为 `true`。
    /// 新窗口首条消息或 `history[0]` 首次出现/漂移重置时为 `false`，handler 层据此将
    /// `prefix_estimated_tokens` 置 0，避免把首条消息尚未建立缓存的 `system + tools` 误计入 `cache_read`。
    pub prefix_cached: bool,
    /// 实际发往上游的 effort（`output_config.effort` / GPT 系 `reasoning.effort`；未发送为 None）
    pub effort: Option<String>,
    /// 实际注入 history[0] 的 thinking 文本标签预算（`max_thinking_length`；未注入为 None）
    pub thinking_budget_tokens: Option<i32>,
    /// 历史中带推理却没能回传上游的 assistant 轮次
    pub history_thinking_loss: HistoryThinkingLoss,
    /// 历史中实际回传上游（`reasoningContent`）的推理签名
    pub history_reasoning_replay: HistoryReasoningReplay,
}

/// 历史中回传上游的推理签名统计，供 credits 反推估算回传推理的输入计费
///
/// 每轮（相邻 assistant 消息合并后）最多回传一块，见 `merge_assistant_messages`。
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct HistoryReasoningReplay {
    /// 回传签名字符数之和
    pub signature_chars: usize,
    /// 回传的推理块数
    pub blocks: usize,
}

/// 历史中带推理却没能回传上游（无 `reasoningContent`）的 assistant 轮次统计
///
/// 一轮指相邻 assistant 消息合并后的一条历史；同一轮只计一次，thinking 块优先于文本化思考。
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct HistoryThinkingLoss {
    /// 带 thinking 块但没有可回传签名（无签名或代理伪造）
    pub unsigned_turns: u32,
    /// 推理以 thinkingAsText 渲染的文本回传，历史中整块剥离
    pub rendered_turns: u32,
    /// 签名已被上游拒绝过（命中拒绝缓存），发送前丢弃
    pub invalid_turns: u32,
}

/// 转换错误
#[derive(Debug)]
pub enum ConversionError {
    UnsupportedModel(String),
    EmptyMessages,
    MissingUserMessage,
    UnsupportedRole { index: usize },
    InvalidSystemContent { index: usize },
}

impl std::fmt::Display for ConversionError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ConversionError::UnsupportedModel(model) => write!(f, "模型不支持: {}", model),
            ConversionError::EmptyMessages => write!(f, "消息列表为空"),
            ConversionError::MissingUserMessage => write!(f, "消息列表缺少 user 消息"),
            ConversionError::UnsupportedRole { index } => {
                write!(f, "messages[{index}].role 仅支持 user、assistant 或 system")
            }
            ConversionError::InvalidSystemContent { index } => {
                write!(
                    f,
                    "messages[{index}].content 必须为字符串或仅含 text 块的 system 内容"
                )
            }
        }
    }
}

impl std::error::Error for ConversionError {}
