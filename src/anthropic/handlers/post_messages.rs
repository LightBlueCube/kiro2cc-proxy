// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! /v1/messages 端点主函数：接收 Anthropic 请求、直通流式转发至 Kiro，并提供请求体调试转储（dump_kiro_request_body）。

use crate::anthropic::converter::{
    ConversionError, Tenant, additional_fields_skipped, convert_request, is_gpt_model,
    is_luna_model,
};
use crate::anthropic::middleware::{ApiKeyContext, AppState};
use crate::anthropic::request_body::RequestBody;
use crate::anthropic::types::ErrorResponse;

use crate::kiro::model::requests::kiro::KiroRequest;
use crate::token;
use axum::{
    Extension,
    extract::State,
    http::StatusCode,
    response::{IntoResponse, Json, Response},
};
use bytes::Bytes;

use super::error::parse_messages_request;
use super::helpers::{
    admit_request, extract_client_ip, extract_user_agent, is_suggestion_mode_request,
    override_thinking_from_model_name, reject_disallowed_model, resolve_thinking_enabled,
    suggestion_mode_response,
};
use super::nonstream::handle_non_stream_request;
use super::stream::handle_stream_request;
use crate::anthropic::websearch;

/// POST /v1/messages
///
/// 创建消息（对话）
pub async fn post_messages(
    State(state): State<AppState>,
    identity: Option<Extension<ApiKeyContext>>,
    axum::extract::ConnectInfo(addr): axum::extract::ConnectInfo<std::net::SocketAddr>,
    headers: axum::http::HeaderMap,
    RequestBody(body): RequestBody,
) -> Response {
    post_messages_as(
        state,
        identity,
        addr,
        headers,
        body,
        crate::model::usage::ApiFormat::AnthropicMessages,
    )
    .await
}

/// `/v1/messages` 的处理主体；OpenAI 端点把请求转成 Anthropic 格式后带上自己的
/// `api_format` 复用（随 usage 记录入库）
pub(crate) async fn post_messages_as(
    state: AppState,
    identity: Option<Extension<ApiKeyContext>>,
    addr: std::net::SocketAddr,
    headers: axum::http::HeaderMap,
    body: Bytes,
    api_format: crate::model::usage::ApiFormat,
) -> Response {
    let mut payload = match parse_messages_request(&body) {
        Ok(p) => p,
        Err(resp) => return resp,
    };

    if let Some(resp) = reject_disallowed_model(identity.as_deref(), &payload.model) {
        return resp;
    }

    // 拦截 Claude Code 输入建议请求（Suggestion Mode）：不转发 Kiro，直接返回空响应。
    // 这类请求携带全量上下文却只产出候选短语，转发会按全价计费且污染 prompt cache。
    // forward_suggestion_mode=true 时放行，正常转发上游（设置页可运行时热切换）。
    if !state
        .forward_suggestion_mode
        .load(std::sync::atomic::Ordering::Relaxed)
        && is_suggestion_mode_request(&payload)
    {
        tracing::info!(
            message_count = %payload.messages.len(),
            "拦截 Suggestion Mode 输入建议请求，返回空响应（不转发上游）"
        );
        return suggestion_mode_response(payload.stream, &payload.model);
    }

    tracing::info!(
        model = %payload.model,
        max_tokens = %payload.max_tokens,
        stream = %payload.stream,
        message_count = %payload.messages.len(),
        "Received POST /v1/messages request"
    );

    // 准入并记录 RPM 与并发（全局 + per-API-Key）；非流式持有到本函数返回，流式随 SSE 流存活
    let api_key_in_flight =
        match admit_request(state.rpm_tracker.as_ref(), identity.as_deref()).await {
            Ok(guard) => guard,
            Err(resp) => return *resp,
        };

    let bound_ids: Vec<u64> = identity
        .as_ref()
        .and_then(|ext| ext.0.bound_credential_ids.clone())
        .unwrap_or_default();

    // 检查 KiroProvider 是否可用
    let provider = match &state.kiro_provider {
        Some(p) => p.clone(),
        None => {
            tracing::error!("KiroProvider 未配置");
            return (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(ErrorResponse::new(
                    "service_unavailable",
                    "Kiro API provider not configured",
                )),
            )
                .into_response();
        }
    };

    // 检测模型名是否包含 "thinking" 后缀，若包含则覆写 thinking 配置
    override_thinking_from_model_name(&mut payload);
    tracing::info!(
        thinking_type = ?payload.thinking.as_ref().map(|t| t.thinking_type.as_str()),
        budget_tokens = ?payload.thinking.as_ref().map(|t| t.budget_tokens),
        "[thinking] 配置"
    );

    // 检查是否为 WebSearch 请求
    if websearch::has_web_search_tool(&payload) {
        tracing::info!("检测到 WebSearch 工具，路由到 WebSearch 处理");

        // 估算输入 tokens
        let input_tokens = token::count_all_tokens(
            payload.model.clone(),
            payload.system.clone(),
            payload.messages.clone(),
            payload.tools.clone(),
        ) as i32;

        return websearch::handle_websearch_request(provider, &payload, input_tokens, &bound_ids)
            .await;
    }

    // 转换请求
    let tenant = identity
        .as_ref()
        .map_or_else(Tenant::anonymous, |ext| Tenant::api_key(ext.0.id));
    let conversion_result = match convert_request(&payload, &tenant, &state.sessions) {
        Ok(result) => result,
        Err(e) => {
            let (error_type, message) = match &e {
                ConversionError::UnsupportedModel(model) => {
                    ("invalid_request_error", format!("模型不支持: {}", model))
                }
                ConversionError::EmptyMessages => {
                    ("invalid_request_error", "消息列表为空".to_string())
                }
                ConversionError::MissingUserMessage
                | ConversionError::UnsupportedRole { .. }
                | ConversionError::InvalidSystemContent { .. } => {
                    ("invalid_request_error", e.to_string())
                }
            };
            tracing::warn!("请求转换失败: {}", e);
            return (
                StatusCode::BAD_REQUEST,
                Json(ErrorResponse::new(error_type, message)),
            )
                .into_response();
        }
    };

    // 是否为 Claude Code /compact 压缩请求（决定上游超时：普通 180s / 压缩 1000s）
    let is_compact_request = conversion_result.is_compact_request;
    // 实际发往上游的 effort 与 thinking 预算（随 usage 记录入库）
    let effort = conversion_result.effort.clone();
    let thinking_budget_tokens = conversion_result.thinking_budget_tokens;
    let history_thinking_loss = conversion_result.history_thinking_loss;
    let history_reasoning_replay = conversion_result.history_reasoning_replay;

    // 构建 Kiro 请求
    let kiro_request = KiroRequest {
        conversation_state: conversion_result.conversation_state,
        profile_arn: state.profile_arn.clone(),
        additional_model_request_fields: conversion_result.additional_model_request_fields,
    };

    let request_body = match serde_json::to_string(&kiro_request) {
        Ok(body) => body,
        Err(e) => {
            tracing::error!("序列化请求失败: {}", e);
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ErrorResponse::new(
                    "internal_error",
                    format!("序列化请求失败: {}", e),
                )),
            )
                .into_response();
        }
    };

    // 请求体可能包含 API Key 与敏感上下文，禁止整包入日志（cr-result C3）

    // 临时取证：dump_kiro_body 配置或 KIRO_DUMP_BODY=1 时把最终发往 Kiro 的 body 写入独立文件（定位后关闭）。
    // dump 涉及目录扫描/排序/删除等同步磁盘 IO，整体挪入 spawn_blocking，不阻塞请求关键路径；
    // 仅在本函数调用一次（provider 层故障转移重试不会重复触发），重试不会产生重复文件
    if kiro_body_dump_enabled() || std::env::var_os("KIRO_DUMP_BODY").is_some_and(|v| v == "1") {
        let request_body_owned = request_body.to_string();
        tokio::task::spawn_blocking(move || dump_kiro_request_body(&request_body_owned));
    }

    let prefix_probe =
        super::helpers::prefix_probe(&state.prefix_cache, &kiro_request.conversation_state);

    // 估算输入 tokens
    // 先取出 thinking_enabled 判断所需字段，避免 payload.tools 等被移动后无法整体借用
    let thinking_enabled = resolve_thinking_enabled(&payload.model, &payload.thinking);
    // 每请求仅此一条 thinking 状态日志（流式 token 转发阶段不再输出）。
    // 双维度区分"客户端请求"与"实际生效"：
    // - 请求：客户端是否携带 thinking（enabled/adaptive），未携带 = 关闭
    // - 生效：请求开启且模型实际支持注入。判定顺序与真实生效链路一致：
    //   luna 强制关闭（resolve_thinking_enabled）→ GPT 系走 reasoning.effort →
    //   4.5 代际 / 第三方模型整体跳过字段（additional_fields_skipped）。
    //   注意必须用 map_model 归一化后的规范名判定——payload.model 是客户端
    //   原始名（如别名 "glm-4.6"），直接传谓词会判不准。
    let requested = payload
        .thinking
        .as_ref()
        .map(|t| t.is_enabled())
        .unwrap_or(false);
    let effective = if !requested {
        "关闭"
    } else if is_luna_model(&payload.model) {
        "不生效(luna不支持)"
    } else if is_gpt_model(&payload.model) {
        "不生效(GPT系走reasoning.effort)"
    } else {
        let mapped = crate::anthropic::converter::map_model(&payload.model)
            .unwrap_or_else(|| payload.model.clone());
        if additional_fields_skipped(&mapped) {
            "不生效(模型不支持该字段)"
        } else {
            "开启"
        }
    };
    tracing::info!(
        model = %payload.model,
        request = if requested { "开启" } else { "关闭" },
        effective,
        "[THINKING] 深度思考状态"
    );
    let input_tokens = token::count_all_tokens(
        payload.model.clone(),
        payload.system,
        payload.messages,
        payload.tools,
    ) as i32;

    // 提取用量追踪信息
    let api_key_id = identity.map(|ext| ext.0.id);
    let usage_tracker = state.usage_tracker.clone();
    let client_ip = extract_client_ip(&headers, Some(&addr));
    let user_agent = extract_user_agent(&headers);

    let json_schema_requested = payload
        .output_config
        .as_ref()
        .and_then(|c| c.format.as_ref())
        .map(|f| f.format_type == "json_schema")
        .unwrap_or(false);

    // 思考文本化仅对 Claude Code 客户端生效（OpenAI 兼容端点等保持原生 thinking 语义）
    let thinking_as_text = crate::anthropic::stream::thinking_as_text_enabled()
        .then(|| crate::anthropic::stream::thinking_text_style(&headers))
        .flatten();

    if payload.stream {
        // 流式响应
        handle_stream_request(
            provider,
            request_body,
            &payload.model,
            input_tokens,
            prefix_probe,
            thinking_enabled,
            usage_tracker,
            api_key_id,
            bound_ids,
            client_ip,
            None, // /v1 无全局 deadline（保持现有行为）
            is_compact_request,
            effort,
            thinking_budget_tokens,
            thinking_as_text,
            user_agent,
            history_thinking_loss,
            history_reasoning_replay,
            api_format,
            api_key_in_flight,
        )
        .await
    } else {
        // 非流式响应
        handle_non_stream_request(
            provider,
            &request_body,
            &payload.model,
            input_tokens,
            prefix_probe,
            usage_tracker,
            api_key_id,
            bound_ids,
            client_ip,
            json_schema_requested,
            is_compact_request,
            effort,
            thinking_budget_tokens,
            user_agent,
            history_thinking_loss,
            history_reasoning_replay,
            api_format,
        )
        .await
    }
}

// 临时取证：把最终发往 Kiro 的完整请求体落盘，dump_kiro_body 配置或 KIRO_DUMP_BODY=1 时启用。
// 文件落在 app/config/kiro-body-dump/（gitignore 目录），权限 0600，定位后关闭。

/// Kiro 请求体落盘取证开关（config `dumpKiroBody`，默认 false）。
///
/// 用 AtomicBool 全局量承载（同 client_token_passthrough 模式）：
/// 判定点在无 &self 的自由函数里，main.rs 启动时按 config 初始化；
/// 环境变量 `KIRO_DUMP_BODY=1` 仍兼容生效（OR 语义）。
static KIRO_BODY_DUMP_ENABLED: std::sync::atomic::AtomicBool =
    std::sync::atomic::AtomicBool::new(false);

/// 设置 Kiro 请求体落盘开关（main.rs 启动时按 config 调用）。
pub fn set_kiro_body_dump_enabled(enabled: bool) {
    KIRO_BODY_DUMP_ENABLED.store(enabled, std::sync::atomic::Ordering::Relaxed);
}

/// 读取 Kiro 请求体落盘开关当前值。
pub fn kiro_body_dump_enabled() -> bool {
    KIRO_BODY_DUMP_ENABLED.load(std::sync::atomic::Ordering::Relaxed)
}

fn dump_kiro_request_body(request_body: &str) {
    use std::io::Write;
    use std::time::{SystemTime, UNIX_EPOCH};

    const MAX_DUMP_FILES: usize = 200;
    const COLLISION_RETRIES: usize = 5;

    let dir = std::path::Path::new("app/config/kiro-body-dump");
    if std::fs::create_dir_all(dir).is_err() {
        tracing::warn!("[BODY-DUMP] 创建 dump 目录失败");
        return;
    }
    // 按文件名排序（毫秒时间戳命名天然按时间有序），保留最近 MAX_DUMP_FILES 个，
    // 防止长时间开启落盘取证后磁盘无限增长
    let mut entries: Vec<std::path::PathBuf> = std::fs::read_dir(dir)
        .map(|rd| {
            rd.filter_map(|e| e.ok())
                .map(|e| e.path())
                .filter(|p| p.is_file() && p.extension().is_some_and(|x| x == "json"))
                .collect()
        })
        .unwrap_or_default();
    if entries.len() >= MAX_DUMP_FILES {
        entries.sort();
        let excess = entries.len() - MAX_DUMP_FILES + 1;
        for stale in &entries[..excess] {
            if let Err(e) = std::fs::remove_file(stale) {
                tracing::warn!(
                    "[BODY-DUMP] 清理旧 dump 文件失败 {}: {}",
                    stale.display(),
                    e
                );
            }
        }
    }
    let ts = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis())
        .unwrap_or_default();
    let mut open = std::fs::OpenOptions::new();
    open.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        open.mode(0o600);
    }
    // 命名与边界对齐：共 MAX_FILENAME_ATTEMPTS 次尝试（含首次），COLLISION_RETRIES 为重试上限
    const MAX_FILENAME_ATTEMPTS: usize = COLLISION_RETRIES + 1;
    let mut path = dir.join(format!("body-{ts}.json"));
    for attempt in 0..MAX_FILENAME_ATTEMPTS {
        path = dir.join(if attempt == 0 {
            format!("body-{ts}.json")
        } else {
            format!("body-{ts}-{attempt}.json")
        });
        let result = open
            .open(&path)
            .and_then(|mut f| f.write_all(request_body.as_bytes()));
        match result {
            Ok(()) => {
                tracing::info!("[BODY-DUMP] 已写入 {}", path.display());
                return;
            }
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => {
                tracing::warn!("[BODY-DUMP] 写入失败: {}", e);
                return;
            }
        }
    }
    tracing::warn!(
        "[BODY-DUMP] 写入失败: 文件名冲突重试 {} 次仍失败（{}）",
        COLLISION_RETRIES,
        path.display()
    );
}

#[cfg(test)]
mod dump_switch_tests {
    use super::*;

    #[test]
    fn test_kiro_body_dump_switch() {
        // 单测试内完成置位/断言/还原：cargo test 默认多线程并行，
        // 拆成两个测试会竞争进程级全局 AtomicBool，存在 flaky 风险
        set_kiro_body_dump_enabled(false);
        assert!(!kiro_body_dump_enabled());
        set_kiro_body_dump_enabled(true);
        assert!(kiro_body_dump_enabled());
        // 还原默认，避免影响其他测试
        set_kiro_body_dump_enabled(false);
        assert!(!kiro_body_dump_enabled());
    }
}
