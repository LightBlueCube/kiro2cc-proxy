// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! 请求体提取：读取失败时留下诊断日志
//!
//! axum 的 `Bytes` 提取器读取失败时直接回 400/413，不记录原因。请求体上传中途断开
//! （客户端到本服务之间的网络中断）时，日志里只剩一行没有前文的 `response status=400`。
//! 本提取器行为与 `Bytes` 一致（含 `DefaultBodyLimit`），只在失败时多记一条 WARN：
//! 已收字节数 / 声明长度、耗时、底层原因（对端关闭还是连接重置）。

use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Instant;

use axum::body::{Body, Bytes};
use axum::extract::{FromRequest, Request, rejection::BytesRejection};
use axum::http::header::CONTENT_LENGTH;
use futures::TryStreamExt;

use super::middleware::ApiKeyContext;

/// 完整读入内存的请求体
pub(crate) struct RequestBody(pub(crate) Bytes);

impl<S: Send + Sync> FromRequest<S> for RequestBody {
    type Rejection = BytesRejection;

    async fn from_request(req: Request, state: &S) -> Result<Self, Self::Rejection> {
        let started = Instant::now();
        let path = req.uri().path().to_string();
        let declared_bytes = req
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|v| v.to_str().ok())
            .map(str::to_string);
        let api_key_id = req.extensions().get::<ApiKeyContext>().map(|c| c.id);

        let received = Arc::new(AtomicUsize::new(0));
        let counter = Arc::clone(&received);
        let req = req.map(|body| {
            Body::from_stream(body.into_data_stream().inspect_ok(move |chunk| {
                counter.fetch_add(chunk.len(), Ordering::Relaxed);
            }))
        });

        let rejection = match Bytes::from_request(req, state).await {
            Ok(bytes) => return Ok(Self(bytes)),
            Err(rejection) => rejection,
        };

        tracing::warn!(
            path = %path,
            api_key_id = ?api_key_id,
            status = rejection.status().as_u16(),
            received_bytes = received.load(Ordering::Relaxed),
            declared_bytes = declared_bytes.as_deref().unwrap_or("-"),
            elapsed_ms = started.elapsed().as_millis() as u64,
            error = %rejection,
            root_cause = %root_cause(&rejection),
            "请求体未能完整读取，请求未进入处理（400 多为上传中途连接断开，413 为超出大小上限）"
        );
        Err(rejection)
    }
}

/// 错误链最底层的原因，如 `connection reset by peer`
fn root_cause(error: &(dyn std::error::Error + 'static)) -> String {
    let mut current = error;
    while let Some(source) = current.source() {
        current = source;
    }
    current.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::StatusCode;

    /// 未挂 `DefaultBodyLimit` 时 axum 的默认上限（2 MB）
    const AXUM_DEFAULT_LIMIT: usize = 2 * 1024 * 1024;

    async fn extract(chunks: Vec<Result<Bytes, std::io::Error>>) -> Result<Bytes, BytesRejection> {
        let body = Body::from_stream(futures::stream::iter(chunks));
        let req = Request::post("/").body(body).unwrap();
        RequestBody::from_request(req, &()).await.map(|b| b.0)
    }

    #[tokio::test]
    async fn complete_body_is_read_in_full() {
        let body = extract(vec![Ok(Bytes::from("abc")), Ok(Bytes::from("defg"))]).await;
        assert_eq!(body.unwrap(), "abcdefg");
    }

    #[tokio::test]
    async fn upload_broken_midway_is_rejected_with_400() {
        let reset = std::io::Error::new(std::io::ErrorKind::ConnectionReset, "connection reset");
        let rejection = extract(vec![Ok(Bytes::from("abc")), Err(reset)])
            .await
            .err()
            .unwrap();
        assert_eq!(rejection.status(), StatusCode::BAD_REQUEST);
        assert_eq!(root_cause(&rejection), "connection reset");
    }

    #[tokio::test]
    async fn body_over_limit_keeps_413() {
        let half = Bytes::from(vec![b'a'; AXUM_DEFAULT_LIMIT / 2 + 1]);
        let rejection = extract(vec![Ok(half.clone()), Ok(half)])
            .await
            .err()
            .unwrap();
        assert_eq!(rejection.status(), StatusCode::PAYLOAD_TOO_LARGE);
    }
}
