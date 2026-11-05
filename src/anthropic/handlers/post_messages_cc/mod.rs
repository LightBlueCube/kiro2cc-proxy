// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! /cc/v1/messages 端点：在标准 /v1/messages 之上附加 300s 全局 deadline，超时强制终止上游并返回错误。

use crate::kiro::model::requests::kiro::KiroRequest;
use crate::token;
use axum::{
    Extension,
    extract::State,
    http::StatusCode,
    response::{IntoResponse, Json, Response},
};
use std::time::Duration;

use crate::anthropic::converter::{ConversionError, Tenant, convert_request};
use crate::anthropic::middleware::{ApiKeyContext, AppState};
use crate::anthropic::request_body::RequestBody;
use crate::anthropic::types::ErrorResponse;

use super::error::parse_messages_request;
use super::helpers::{
    admit_request, extract_client_ip, extract_user_agent, is_suggestion_mode_request,
    override_thinking_from_model_name, reject_disallowed_model, resolve_thinking_enabled,
    suggestion_mode_response,
};
use super::nonstream::handle_non_stream_request;
use super::stream::handle_stream_request;
use crate::anthropic::websearch;

/// /cc/v1/messages 流式响应全局 deadline（秒），防止上游挂起导致请求永不结束
const CC_STREAM_DEADLINE_SECS: u64 = 300;

/// POST /cc/v1/messages
///
/// Claude Code 兼容端点，与 /v1/messages 的区别在于：
/// - 流式响应带 300s 全局 deadline，防止上游挂起导致请求永不结束（/v1 无此限制）
///
/// 其余行为与 /v1/messages 完全一致：同为实时转发，`message_start` 先给估算
/// `input_tokens`，末尾 `message_delta` 给出终值。
pub async fn post_messages_cc(
    State(state): State<AppState>,
    identity: Option<Extension<ApiKeyContext>>,
    axum::extract::ConnectInfo(addr): axum::extract::ConnectInfo<std::net::SocketAddr>,
    headers: axum::http::HeaderMap,
    RequestBody(body): RequestBody,
) -> Response {
    let mut payload = match parse_messages_request(&body) {
        Ok(p) => p,
        Err(resp) => return resp,
    };

    if let Some(resp) = reject_disallowed_model(identity.as_deref(), &payload.model) {
        return resp;
    }

    // 拦截 Claude Code 输入建议请求（Suggestion Mode）：不转发 Kiro，直接返回空响应。
    // forward_suggestion_mode=true 时放行（设置页可运行时热切换），正常转发上游。
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
        "Received POST /cc/v1/messages request"
    );

    // 准入并记录 RPM 与并发（与 /v1/messages 同口径）；非流式持有到本函数返回，流式随 SSE 流存活
    let api_key_in_flight =
        match admit_request(state.rpm_tracker.as_ref(), identity.as_deref()).await {
            Ok(guard) => guard,
            Err(resp) => return *resp,
        };

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

    let bound_ids: Vec<u64> = identity
        .as_ref()
        .and_then(|ext| ext.0.bound_credential_ids.clone())
        .unwrap_or_default();

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

    // 请求体可能包含 API Key 与敏感上下文，禁止整包入日志（cr-result C4）

    let prefix_probe =
        super::helpers::prefix_probe(&state.prefix_cache, &kiro_request.conversation_state);

    // 估算输入 tokens
    // 先取出 thinking_enabled 判断所需字段，避免 payload.tools 等被移动后无法整体借用
    let thinking_enabled = resolve_thinking_enabled(&payload.model, &payload.thinking);
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

    // 思考文本化仅对 Claude Code 客户端生效
    let thinking_as_text = crate::anthropic::stream::thinking_as_text_enabled()
        .then(|| crate::anthropic::stream::thinking_text_style(&headers))
        .flatten();

    if payload.stream {
        // 流式响应：与 /v1 相同的实时转发，额外带 300s 全局 deadline
        // （上游挂起保护，沿用此端点历史上一直具备的 5min 上限）
        handle_stream_request(
            provider,
            request_body,
            &payload.model,
            input_tokens,
            prefix_probe,
            thinking_enabled,
            usage_tracker.clone(),
            api_key_id,
            bound_ids,
            client_ip,
            Some(Duration::from_secs(CC_STREAM_DEADLINE_SECS)),
            is_compact_request,
            effort,
            thinking_budget_tokens,
            thinking_as_text,
            user_agent,
            history_thinking_loss,
            history_reasoning_replay,
            crate::model::usage::ApiFormat::CcMessages,
            api_key_in_flight,
        )
        .await
    } else {
        // 非流式响应（复用现有逻辑，已经使用正确的 input_tokens）
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
            crate::model::usage::ApiFormat::CcMessages,
        )
        .await
    }
}
