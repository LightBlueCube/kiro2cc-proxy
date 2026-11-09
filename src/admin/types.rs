// Copyright (c) 2026 Harllan He. Licensed under MIT.
//! Admin API 请求/响应类型：凭据状态、启停请求、余额、模型条目、负载均衡模式等 DTO 定义。

use serde::{Deserialize, Serialize};

// ============ 账号状态 ============

/// 所有账号状态响应
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CredentialsStatusResponse {
    /// 账号总数
    pub total: usize,
    /// 可用账号数量（未禁用）
    pub available: usize,
    /// 当前活跃账号 ID
    pub current_id: u64,
    /// 各账号状态列表
    pub credentials: Vec<CredentialStatusItem>,
}

/// 单个账号的状态信息
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CredentialStatusItem {
    /// 账号唯一 ID
    pub id: u64,
    /// 优先级（数字越小优先级越高）
    pub priority: u32,
    /// 是否被禁用
    pub disabled: bool,
    /// 连续失败次数
    pub failure_count: u32,
    /// 是否为当前活跃账号
    pub is_current: bool,
    /// Token 过期时间（RFC3339 格式）
    pub expires_at: Option<String>,
    /// 认证方式
    pub auth_method: Option<String>,
    /// 是否有 Profile ARN
    pub has_profile_arn: bool,
    /// OIDC Client ID（非机密；编辑弹窗只读展示）
    #[serde(skip_serializing_if = "Option::is_none")]
    pub client_id: Option<String>,
    /// Profile ARN（非机密；编辑弹窗只读展示）
    #[serde(skip_serializing_if = "Option::is_none")]
    pub profile_arn: Option<String>,
    /// 账号级 Machine ID（非机密；编辑弹窗只读展示）
    #[serde(skip_serializing_if = "Option::is_none")]
    pub machine_id: Option<String>,
    /// refreshToken 的 SHA-256 哈希（用于前端重复检测）
    pub refresh_token_hash: Option<String>,
    /// 用户邮箱（用于前端显示）
    pub email: Option<String>,
    /// 用户昵称/备注名（用于前端显示）
    pub nickname: Option<String>,
    /// 登录来源 / IdP 标识（social 账号存 Google / GitHub；external_idp 存 AzureAD 等）
    #[serde(skip_serializing_if = "Option::is_none")]
    pub provider: Option<String>,
    /// 账号级 Auth Region（用于 Token 刷新；未配置时回退全局）
    #[serde(skip_serializing_if = "Option::is_none")]
    pub auth_region: Option<String>,
    /// 账号级 API Region（用于 API 请求；未配置时回退全局）
    #[serde(skip_serializing_if = "Option::is_none")]
    pub api_region: Option<String>,
    pub success_count: u64,
    /// 最后一次 API 调用时间（RFC3339 格式）
    pub last_used_at: Option<String>,
    /// 是否配置了账号级代理
    pub has_proxy: bool,
    /// 代理 URL（用于前端展示）
    #[serde(skip_serializing_if = "Option::is_none")]
    pub proxy_url: Option<String>,
    /// 健康状态
    pub health_status: crate::kiro::token_manager::HealthStatus,
    /// 健康状态的判定依据（近 10 分钟限流 / 空响应、连续失败、冷却剩余）
    #[serde(flatten)]
    pub health_detail: crate::kiro::token_manager::HealthDetail,
    /// 最近 7 天限流次数（429 响应，滚动 7×24 小时窗口）
    pub throttle_count: u64,
    /// 禁用原因（None = 未禁用；QuotaExceeded 时前端显示「额度已用尽」而非通用「已禁用」）
    #[serde(skip_serializing_if = "Option::is_none")]
    pub disabled_reason: Option<crate::kiro::token_manager::DisabledReason>,
    /// 本轮连续失败中最近一次账号报错（状态码、原文、时间）；None = 无
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_error: Option<crate::kiro::token_manager::AccountError>,
    /// 因连续拒答处于冷却期的模型（期间该模型的请求换用其它账号）
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub model_restrictions: Vec<crate::kiro::token_manager::ModelRestriction>,
}

// ============ 操作请求 ============

/// 启用/禁用账号请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetDisabledRequest {
    /// 是否禁用
    pub disabled: bool,
}

/// 修改优先级请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetPriorityRequest {
    /// 新优先级值
    pub priority: u32,
}

/// 添加账号请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct AddCredentialRequest {
    /// 刷新令牌（必填）
    pub refresh_token: String,

    /// 认证方式（可选，默认 social）
    #[serde(default = "default_auth_method")]
    pub auth_method: String,

    /// OIDC Client ID（IdC / external_idp 认证需要）
    pub client_id: Option<String>,

    /// OIDC Client Secret（IdC 认证需要；external_idp 公共客户端不填）
    pub client_secret: Option<String>,

    /// IdP 标识（如 "AzureAD"），仅 external_idp 使用
    pub provider: Option<String>,

    /// IdP token 刷新端点（external_idp 必填）
    pub token_endpoint: Option<String>,

    /// OAuth2 scope（external_idp 使用；需含 offline_access）
    pub scopes: Option<String>,

    /// Profile ARN（可选，企业版 IdC 账号调用 Q 端点必需，Social 刷新会自动获取）
    pub profile_arn: Option<String>,

    /// 优先级（可选，默认 0）
    #[serde(default)]
    pub priority: u32,

    /// 是否以禁用状态创建（可选，默认 false）
    #[serde(default)]
    pub disabled: bool,

    /// 账号级 Region 配置（用于 OIDC token 刷新）
    /// 未配置时回退到 config.json 的全局 region
    pub region: Option<String>,

    /// 账号级 Auth Region（用于 Token 刷新）
    pub auth_region: Option<String>,

    /// 账号级 API Region（用于 API 请求）
    pub api_region: Option<String>,

    /// 账号级 Machine ID（可选，64 位字符串）
    /// 未配置时回退到 config.json 的 machineId
    pub machine_id: Option<String>,

    /// 用户邮箱（可选，用于前端显示）
    pub email: Option<String>,

    /// 用户昵称/备注名（可选，用于前端显示）
    pub nickname: Option<String>,

    /// 账号级代理 URL（可选，特殊值 "direct" 表示不使用代理）
    pub proxy_url: Option<String>,

    /// 账号级代理认证用户名（可选）
    pub proxy_username: Option<String>,

    /// 账号级代理认证密码（可选）
    pub proxy_password: Option<String>,
}

fn default_auth_method() -> String {
    "social".to_string()
}

#[cfg(test)]
mod tests {
    use super::{AddCredentialRequest, CredentialStatusItem};

    /// 构造仅填充必填字段的状态项，其余用默认值
    fn sample_status_item() -> CredentialStatusItem {
        CredentialStatusItem {
            id: 1,
            priority: 0,
            disabled: false,
            failure_count: 0,
            is_current: false,
            expires_at: None,
            auth_method: Some("social".to_string()),
            has_profile_arn: true,
            client_id: None,
            profile_arn: None,
            machine_id: None,
            refresh_token_hash: None,
            email: None,
            nickname: None,
            provider: None,
            auth_region: None,
            api_region: None,
            success_count: 0,
            last_used_at: None,
            has_proxy: false,
            proxy_url: None,
            health_status: crate::kiro::token_manager::HealthStatus::Healthy,
            health_detail: Default::default(),
            throttle_count: 0,
            disabled_reason: None,
            last_error: None,
            model_restrictions: Vec::new(),
        }
    }

    /// 登录来源与区域配置需随状态列表下发，前端弹窗据此展示
    #[test]
    fn credential_status_item_serializes_provider_and_regions() {
        let mut item = sample_status_item();
        item.provider = Some("Google".to_string());
        item.auth_region = Some("us-east-1".to_string());
        item.api_region = Some("us-west-2".to_string());

        let json = serde_json::to_value(&item).unwrap();

        assert_eq!(json["provider"], "Google");
        assert_eq!(json["authRegion"], "us-east-1");
        assert_eq!(json["apiRegion"], "us-west-2");
    }

    /// Client ID / Profile ARN / Machine ID（均非机密）需随状态列表下发，
    /// 编辑弹窗「有值即锁定」只读展示依赖这三个字段
    #[test]
    fn credential_status_item_serializes_idc_fields_for_edit_lock() {
        let mut item = sample_status_item();
        item.client_id = Some("client-id".to_string());
        item.profile_arn = Some("arn:aws:codewhisperer:us-east-1:123:profile/abc".to_string());
        item.machine_id = Some("0a1b2c3d".repeat(8));

        let json = serde_json::to_value(&item).unwrap();

        assert_eq!(json["clientId"], "client-id");
        assert!(
            json["profileArn"]
                .as_str()
                .is_some_and(|v| v.starts_with("arn:aws:"))
        );
        assert_eq!(json["machineId"], "0a1b2c3d".repeat(8));
    }

    /// 未配置时省略字段而非输出 null，保持与结构体既有可选字段一致的下发形状
    #[test]
    fn credential_status_item_omits_empty_provider_and_regions() {
        let json = serde_json::to_value(sample_status_item()).unwrap();

        assert!(json.get("provider").is_none());
        assert!(json.get("authRegion").is_none());
        assert!(json.get("apiRegion").is_none());
        assert!(json.get("clientId").is_none());
        assert!(json.get("profileArn").is_none());
        assert!(json.get("machineId").is_none());
    }

    #[test]
    fn add_credential_request_defaults_disabled_to_false() {
        let request: AddCredentialRequest =
            serde_json::from_str(r#"{"refreshToken":"token"}"#).unwrap();

        assert!(!request.disabled);
    }

    #[test]
    fn add_credential_request_accepts_disabled_true() {
        let request: AddCredentialRequest =
            serde_json::from_str(r#"{"refreshToken":"token","disabled":true}"#).unwrap();

        assert!(request.disabled);
    }
}

/// 添加账号成功响应
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AddCredentialResponse {
    pub success: bool,
    pub message: String,
    /// 新添加的账号 ID
    pub credential_id: u64,
    /// 用户邮箱（如果获取成功）
    #[serde(skip_serializing_if = "Option::is_none")]
    pub email: Option<String>,
}

/// 更新账号请求（所有字段可选，只更新提供的字段）
#[derive(Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateCredentialRequest {
    /// 刷新令牌（可选，更新后会重新验证）
    pub refresh_token: Option<String>,

    /// 认证方式（可选）
    pub auth_method: Option<String>,

    /// OIDC Client ID（可选）
    pub client_id: Option<String>,

    /// OIDC Client Secret（可选）
    pub client_secret: Option<String>,

    /// Profile ARN（可选，企业版 IdC 账号调用 Q 端点必需；传空字符串表示清除该字段）
    pub profile_arn: Option<String>,

    /// 账号级 Auth Region（用于 Token 刷新）
    pub auth_region: Option<String>,

    /// 账号级 API Region（用于 API 请求）
    pub api_region: Option<String>,

    /// 账号级 Machine ID（可选）
    pub machine_id: Option<String>,

    /// 用户邮箱（可选，用于前端显示）
    pub email: Option<String>,

    /// 用户昵称/备注名（可选，用于前端显示）
    pub nickname: Option<String>,

    /// 账号级代理 URL（可选）
    pub proxy_url: Option<String>,

    /// 账号级代理认证用户名（可选）
    pub proxy_username: Option<String>,

    /// 账号级代理认证密码（可选）
    pub proxy_password: Option<String>,

    /// 登录来源 / IdP 标识（可选；social 账号存 Google / GitHub）
    /// 传空字符串表示清除该字段
    pub provider: Option<String>,
}

// ============ 余额查询 ============

/// 余额查询响应
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct BalanceResponse {
    /// 账号 ID
    pub id: u64,
    /// 订阅类型
    pub subscription_title: Option<String>,
    /// 当前使用量
    pub current_usage: f64,
    /// 使用限额
    pub usage_limit: f64,
    /// 剩余额度
    pub remaining: f64,
    /// 使用百分比
    pub usage_percentage: f64,
    /// 下次重置时间（Unix 时间戳）
    pub next_reset_at: Option<f64>,
}

// ============ 负载均衡配置 ============

/// 负载均衡模式响应
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LoadBalancingModeResponse {
    /// 当前模式（"priority" 或 "balanced"）
    pub mode: String,
}

/// 设置负载均衡模式请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetLoadBalancingModeRequest {
    /// 模式（"priority" 或 "balanced"）
    pub mode: String,
}

/// Suggestion Mode 放行开关查询响应
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SuggestionModeResponse {
    /// 是否放行 Suggestion Mode 输入建议请求
    pub enabled: bool,
}

/// 设置 Suggestion Mode 放行开关请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetSuggestionModeRequest {
    /// 是否放行 Suggestion Mode 输入建议请求
    pub enabled: bool,
}

/// 客户端 token 直通开关查询响应
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ClientTokenPassthroughResponse {
    /// 是否直通真实 usage（true 1:1 上报，false 按 0.6657 展示缩放）
    pub enabled: bool,
}

/// 设置客户端 token 直通开关请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetClientTokenPassthroughRequest {
    /// 是否直通真实 usage（true 1:1 上报，false 按 0.6657 展示缩放）
    pub enabled: bool,
}

/// 思考文本化开关查询响应
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ThinkingAsTextResponse {
    /// 是否把 Claude Code 的 thinking 块改写为引用文本逐行展示
    pub enabled: bool,
}

/// 设置思考文本化开关请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetThinkingAsTextRequest {
    /// 是否把 Claude Code 的 thinking 块改写为引用文本逐行展示
    pub enabled: bool,
}

/// Kiro 请求体落盘取证开关查询响应
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DumpKiroBodyResponse {
    /// 是否把发往 Kiro 的请求体落盘到 app/config/kiro-body-dump/
    pub enabled: bool,
}

/// 设置 Kiro 请求体落盘取证开关请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetDumpKiroBodyRequest {
    /// 是否把发往 Kiro 的请求体落盘到 app/config/kiro-body-dump/
    pub enabled: bool,
}

/// DEBUG 日志开关查询响应
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DebugLogsResponse {
    /// 是否输出 DEBUG 级别日志（控制台 / 日志文件 / Admin 日志页三处一致）
    pub enabled: bool,
}

/// 设置 DEBUG 日志开关请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetDebugLogsRequest {
    /// 是否输出 DEBUG 级别日志
    pub enabled: bool,
}

/// 运行时配置（maxRpm / port / proxyUrl / writeChunkingMaxLines）查询响应
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeConfigResponse {
    /// 每账号 RPM 上限（运行时当前值，0 = 不限，热生效）
    pub max_rpm_per_credential: u32,
    /// 每账号 RPM 软上限（运行时当前值，0 = 关闭，热生效）
    pub soft_rpm_per_credential: u32,
    /// 软上限是否允许新会话溢出到下一优先级（运行时当前值，热生效）
    pub soft_rpm_spill_to_lower_priority: bool,
    /// 每账号并发上限（运行时当前值，0 = 不限，热生效）
    pub max_concurrency_per_credential: u32,
    /// 监听端口（config.json 持久化值，重启后生效）
    pub port: u16,
    /// 上游代理地址（config.json 持久化值，重启后生效；空表示未配置）
    pub proxy_url: Option<String>,
    /// 分块写入约束的行数上限（运行时当前值，0 = 不追加约束，热生效）
    pub write_chunking_max_lines: u32,
    /// 对话请求走的上游 API（config.json 持久化值，重启后生效）
    pub upstream_api: crate::model::config::UpstreamApi,
}

/// 设置运行时配置请求（字段均为可选，仅更新显式传入的字段）
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetRuntimeConfigRequest {
    /// 每账号 RPM 上限（0 = 不限，热生效）
    pub max_rpm_per_credential: Option<u32>,
    /// 每账号 RPM 软上限（0 = 关闭，热生效）
    pub soft_rpm_per_credential: Option<u32>,
    /// 软上限是否允许新会话溢出到下一优先级（热生效）
    pub soft_rpm_spill_to_lower_priority: Option<bool>,
    /// 每账号并发上限（0 = 不限，热生效）
    pub max_concurrency_per_credential: Option<u32>,
    /// 监听端口（1–65535，重启后生效）
    pub port: Option<u16>,
    /// 上游代理地址（空串/None = 清除；支持 http/https/socks5，重启后生效）
    pub proxy_url: Option<String>,
    /// 分块写入约束的行数上限（0 = 不追加约束，热生效，仅影响之后的新请求）
    pub write_chunking_max_lines: Option<u32>,
    /// 对话请求走的上游 API（重启后生效）
    pub upstream_api: Option<crate::model::config::UpstreamApi>,
}

// ============ 通用响应 ============

/// 操作成功响应
#[derive(Debug, Serialize)]
pub struct SuccessResponse {
    pub success: bool,
    pub message: String,
}

impl SuccessResponse {
    pub fn new(message: impl Into<String>) -> Self {
        Self {
            success: true,
            message: message.into(),
        }
    }
}

// ============ API Key 管理 ============

/// 创建 API Key 请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateApiKeyRequest {
    /// 备注名称（如 "张三-月付"）
    pub name: String,
    /// 过期时间（可选，ISO 8601 格式）— 按日期模式
    #[serde(default)]
    pub expires_at: Option<chrono::DateTime<chrono::Utc>>,
    /// 额度限制数值 — 按额度模式
    #[serde(default)]
    pub spending_limit: Option<f64>,
    /// 额度计量单位（"usd" | "credits"），不传默认 "usd"
    #[serde(default)]
    pub limit_unit: Option<String>,
    /// 有效期天数（懒激活模式）
    #[serde(default)]
    pub duration_days: Option<f64>,
    /// 绑定的账号 ID 列表
    #[serde(default)]
    pub bound_credential_ids: Option<Vec<u64>>,
    /// 可调用模型白名单（空或缺省表示不限制）
    #[serde(default)]
    pub allowed_models: Option<Vec<String>>,
    /// 每分钟请求数上限（缺省 0 = 不限）
    #[serde(default)]
    pub rpm_limit: u32,
    /// 同时在途请求数上限（缺省 0 = 不限）
    #[serde(default)]
    pub concurrency_limit: u32,
}

/// 更新 API Key 请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateApiKeyRequest {
    /// 备注名称
    #[serde(default)]
    pub name: Option<String>,
    /// 启用状态
    #[serde(default)]
    pub enabled: Option<bool>,
    /// 过期时间（null 表示永不过期）
    #[serde(default, deserialize_with = "deserialize_optional_datetime")]
    pub expires_at: Option<Option<chrono::DateTime<chrono::Utc>>>,
    /// 额度限制（null 表示不限额）
    #[serde(default, deserialize_with = "deserialize_optional_f64")]
    pub spending_limit: Option<Option<f64>>,
    /// 额度计量单位（"usd" | "credits"）
    #[serde(default)]
    pub limit_unit: Option<String>,
    /// 有效期天数（懒激活模式）
    #[serde(default, deserialize_with = "deserialize_optional_f64")]
    pub duration_days: Option<Option<f64>>,
    /// 绑定的账号 ID 列表（null 表示清除绑定）
    #[serde(default, deserialize_with = "deserialize_optional_vec_u64")]
    pub bound_credential_ids: Option<Option<Vec<u64>>>,
    /// 可调用模型白名单（null 表示清除限制）
    #[serde(default, deserialize_with = "deserialize_optional_vec_string")]
    pub allowed_models: Option<Option<Vec<String>>>,
    /// 每分钟请求数上限（0 = 不限）
    #[serde(default)]
    pub rpm_limit: Option<u32>,
    /// 同时在途请求数上限（0 = 不限）
    #[serde(default)]
    pub concurrency_limit: Option<u32>,
}

/// 区分 JSON 中"字段缺失"与"字段为 null"
/// 缺失 → None（不更新），null → Some(None)（永不过期），有值 → Some(Some(dt))
fn deserialize_optional_datetime<'de, D>(
    deserializer: D,
) -> Result<Option<Option<chrono::DateTime<chrono::Utc>>>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    Option::deserialize(deserializer).map(Some)
}

/// 区分 JSON 中"字段缺失"与"字段为 null"（Vec<u64> 版本）
/// 缺失 → None（不更新），null → Some(None)（清除绑定），有值 → Some(Some(ids))
fn deserialize_optional_vec_u64<'de, D>(
    deserializer: D,
) -> Result<Option<Option<Vec<u64>>>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    Option::deserialize(deserializer).map(Some)
}

/// 区分 JSON 中"字段缺失"与"字段为 null"（Vec<String> 版本）
/// 缺失 → None（不更新），null → Some(None)（清除限制），有值 → Some(Some(models))
fn deserialize_optional_vec_string<'de, D>(
    deserializer: D,
) -> Result<Option<Option<Vec<String>>>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    Option::deserialize(deserializer).map(Some)
}

/// 区分 JSON 中"字段缺失"与"字段为 null"（f64 版本）
/// 缺失 → None（不更新），null → Some(None)（不限额），有值 → Some(Some(limit))
fn deserialize_optional_f64<'de, D>(deserializer: D) -> Result<Option<Option<f64>>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    Option::deserialize(deserializer).map(Some)
}

/// 错误响应
#[derive(Debug, Serialize)]
pub struct AdminErrorResponse {
    pub error: AdminError,
}

#[derive(Debug, Serialize)]
pub struct AdminError {
    #[serde(rename = "type")]
    pub error_type: String,
    pub message: String,
}

impl AdminErrorResponse {
    pub fn new(error_type: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            error: AdminError {
                error_type: error_type.into(),
                message: message.into(),
            },
        }
    }

    pub fn invalid_request(message: impl Into<String>) -> Self {
        Self::new("invalid_request", message)
    }

    pub fn authentication_error() -> Self {
        Self::new("authentication_error", "Invalid or missing admin password")
    }

    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new("not_found", message)
    }

    pub fn api_error(message: impl Into<String>) -> Self {
        Self::new("api_error", message)
    }

    pub fn internal_error(message: impl Into<String>) -> Self {
        Self::new("internal_error", message)
    }
}

// ============ 认证密钥管理 ============

/// 认证密钥查询响应（脱敏）
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AuthKeysResponse {
    /// Admin Password（脱敏显示）
    pub admin_psw: String,
}

/// 修改认证密钥请求
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SetAuthKeysRequest {
    /// 新的 Admin Password（可选，不传则不修改）
    #[serde(default, alias = "adminApiKey")]
    pub admin_psw: Option<String>,
}

// ============ 支持模型 ============

/// 支持模型条目（在 /v1/models 的 Model 基础上附加官方费率倍率）
#[derive(Debug, Serialize)]
#[cfg_attr(test, derive(Deserialize))]
pub struct AdminModelItem {
    #[serde(flatten)]
    pub model: crate::anthropic::types::Model,
    /// 官方费率倍率（实时查询，无法匹配或上游调用失败时为 None）
    pub rate_multiplier: Option<f64>,
    /// 模型发布日期（`YYYY-MM-DD`）；未登记为 None，前端据此实时计算「最近新增」标记
    pub released_at: Option<String>,
    /// 官方模型描述；上游未返回或回退静态表时为 None
    pub description: Option<String>,
}

/// 支持模型列表响应
#[derive(Debug, Serialize)]
#[cfg_attr(test, derive(Deserialize))]
pub struct AdminModelsResponse {
    pub object: String,
    pub data: Vec<AdminModelItem>,
}

// ============ 更新日志 ============

/// 中英双语文案
#[derive(Debug, Serialize)]
#[cfg_attr(test, derive(Deserialize))]
pub struct AdminBilingualText {
    pub zh: String,
    pub en: String,
}

/// 更新日志分类分组
#[derive(Debug, Serialize)]
#[cfg_attr(test, derive(Deserialize))]
pub struct AdminReleaseNoteGroup {
    pub title_zh: String,
    pub title_en: String,
    pub items: Vec<AdminBilingualText>,
}

/// 单个版本的更新日志条目
#[derive(Debug, Serialize)]
#[cfg_attr(test, derive(Deserialize))]
pub struct AdminReleaseNote {
    pub version: String,
    pub is_latest: bool,
    pub groups: Vec<AdminReleaseNoteGroup>,
}

/// 更新日志列表响应
#[derive(Debug, Serialize)]
#[cfg_attr(test, derive(Deserialize))]
pub struct AdminReleaseNotesResponse {
    pub object: String,
    pub data: Vec<AdminReleaseNote>,
}
