// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! Kiro API 客户端模块：HTTP provider 与多账号故障转移、端点限流桶、OAuth token 管理、二进制事件流解析（parser）。

pub mod affinity;
pub mod endpoint;
pub mod machine_id;
pub mod model;
pub mod parser;
pub mod provider;
pub mod token_manager;
