// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 上游响应体 → Kiro 事件
//!
//! 按上游 API 选择解码方式，对调用方统一输出 [`Event`]：
//! - GenerateAssistantResponse：AWS Event Stream 二进制帧（[`EventStreamDecoder`]）
//! - CreateResponse：SSE（[`SseEventDecoder`]）

use crate::kiro::create_response::SseEventDecoder;
use crate::kiro::endpoint::UpstreamApi;
use crate::kiro::model::events::{Event, EventType};
use crate::kiro::parser::decoder::EventStreamDecoder;
use crate::kiro::parser::error::ParseError;

/// 解码失败
#[derive(Debug)]
pub enum DecodeError {
    /// 流层面的错误（帧损坏、缓冲区溢出）；致命与否由错误种类决定
    Stream(ParseError),
    /// 单个事件无法解析，事件被丢弃；`tool_use` 表示丢的是工具调用
    Event { tool_use: bool, error: ParseError },
}

pub struct UpstreamDecoder {
    inner: Inner,
}

enum Inner {
    EventStream(EventStreamDecoder),
    Sse(SseEventDecoder),
}

impl UpstreamDecoder {
    pub fn new(api: UpstreamApi) -> Self {
        let inner = match api {
            UpstreamApi::GenerateAssistantResponse => Inner::EventStream(EventStreamDecoder::new()),
            UpstreamApi::CreateResponse => Inner::Sse(SseEventDecoder::new()),
        };
        Self { inner }
    }

    /// 喂入一段响应体，返回其中完整解出的事件
    pub fn decode(&mut self, chunk: &[u8]) -> Vec<Result<Event, DecodeError>> {
        let decoder = match &mut self.inner {
            Inner::Sse(decoder) => return decoder.feed(chunk),
            Inner::EventStream(decoder) => decoder,
        };

        let mut out = Vec::new();
        if let Err(e) = decoder.feed(chunk) {
            out.push(Err(DecodeError::Stream(e)));
        }
        for result in decoder.decode_iter() {
            let frame = match result {
                Ok(frame) => frame,
                Err(e) => {
                    out.push(Err(DecodeError::Stream(e)));
                    continue;
                }
            };
            let tool_use = frame.event_type() == Some(EventType::ToolUse.as_str());
            out.push(
                Event::from_frame(frame).map_err(|error| DecodeError::Event { tool_use, error }),
            );
        }
        out
    }

    /// 响应体结束：处理残留数据
    pub fn finish(&mut self) -> Vec<Result<Event, DecodeError>> {
        match &mut self.inner {
            Inner::Sse(decoder) => decoder.finish(),
            // 半帧无法解码，由 truncated 报告
            Inner::EventStream(_) => Vec::new(),
        }
    }

    /// 响应体在 [`Self::finish`] 后仍不完整：残留半帧，或 CreateResponse 缺少终止事件
    pub fn truncated(&self) -> bool {
        match &self.inner {
            Inner::EventStream(decoder) => decoder.buffer_len() > 0,
            Inner::Sse(decoder) => decoder.truncated(),
        }
    }
}
