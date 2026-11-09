// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! Kiro CreateResponse API（`POST /v1/responses`）协议适配
//!
//! 协议取自 Kiro CLI 3.1.0 内置的 KAS agent（`buildCreateResponseRequest` /
//! `sendCreateResponseRaw` / `parseCreateResponseSse`）。上游 API 的选择对 converter 与
//! 下游状态机透明：请求仍按 GenerateAssistantResponse 构建，发送时在此翻译；响应解成同一套
//! [`crate::kiro::model::events::Event`]。
//! - `request`: KiroRequest → CreateResponse 请求体
//! - `sse`: CreateResponse SSE → Kiro 事件

mod request;
mod sse;

pub use request::{CreateResponseRequest, to_create_response_request};
pub use sse::SseEventDecoder;
