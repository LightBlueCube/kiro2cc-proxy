//! 单凭据条目：健康状态（HealthStatus）、禁用原因（DisabledReason）与统计计数。
use super::probe::ProbeBackoff;
use super::trial::TrialSlot;
use super::unreachable::FailureCooldown;
use super::upstream_failures::UpstreamFailures;
use crate::kiro::model::credentials::KiroCredentials;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::time::Instant;

// 多账号 Token 管理器
// ============================================================================

/// 单个账号条目的状态
pub(crate) struct CredentialEntry {
    /// 账号唯一 ID
    pub(crate) id: u64,
    /// 账号信息
    pub(crate) credentials: KiroCredentials,
    /// API 调用连续 401/403 次数
    pub(crate) failure_count: u32,
    /// Token 刷新连续失败次数（独立于 failure_count，语义为"刷新失败"而非"API 调用失败"）
    pub(crate) refresh_failure_count: u32,
    /// 是否已禁用
    pub(crate) disabled: bool,
    /// 禁用原因（区分手动禁用与各类自动禁用）
    pub(crate) disabled_reason: Option<DisabledReason>,
    /// API 调用成功次数
    pub(crate) success_count: u64,
    /// 最后一次 API 调用时间（RFC3339 格式）
    pub(crate) last_used_at: Option<String>,
    /// 被限流次数（429 响应，累计）
    pub(crate) throttle_count: u64,
    /// 近期上游失败（限流、空响应），健康度据此判定；仅内存
    pub(crate) upstream_failures: UpstreamFailures,
    /// 请求发送失败（代理错误、连接失败、超时）的连续计数与冷却；仅内存
    pub(crate) unreachable: FailureCooldown,
    /// 冷却期间的试探间隔；仅内存
    pub(crate) probe: ProbeBackoff,
    /// 冷却结束后等一次完整成功的响应才回到健康（待验证）：进入冷却时置位，
    /// 正常完成一次响应或手动恢复时清除；仅内存
    pub(crate) awaiting_verification: bool,
    /// 待验证期间的试验名额，同一时刻只放行一个请求；仅内存
    pub(crate) trial: TrialSlot,
    /// 按模型（小写）的连续拒答与冷却；仅内存
    pub(crate) model_refusals: HashMap<String, ModelRefusals>,
    /// 最后一次被限流时间（UTC，随统计持久化；健康度不使用）
    pub(crate) last_throttled_wall: Option<DateTime<Utc>>,
    /// 最后一次 token 刷新时间（用于冷却期控制）
    pub(crate) last_refreshed_at: Option<Instant>,
    /// 轮转偏移量：429 时 +1，成功时清零；选择账号时优先选 bias 最小的
    pub(crate) rotation_bias: u32,
    /// 被判定额度用尽的时间（UTC，持久化），用于跨自然月后自动恢复
    pub(crate) quota_exhausted_at: Option<DateTime<Utc>>,
    /// 本轮连续失败中最近一次账号报错（请求失败或 Token 刷新失败，持久化）；
    /// 正常完成一次响应或手动启用时清除
    pub(crate) last_error: Option<AccountError>,
}

/// 账号某个模型的连续拒答
#[derive(Debug, Default)]
pub(crate) struct ModelRefusals {
    pub(crate) cooldown: FailureCooldown,
    /// 最近一次拒答的上游说明原文
    pub(crate) message: String,
}

impl CredentialEntry {
    /// 清空运行时健康状态（失败计数、冷却、试探、待验证、模型拒答、最近报错）
    ///
    /// 不碰禁用状态、额度耗尽标记与累计计数：启用账号是另一个操作。
    pub(crate) fn reset_health(&mut self) {
        self.failure_count = 0;
        self.refresh_failure_count = 0;
        self.upstream_failures.reset();
        self.unreachable.reset();
        self.probe.reset();
        self.awaiting_verification = false;
        self.model_refusals.clear();
        self.rotation_bias = 0;
        self.last_error = None;
    }
}

/// 账号报错原文（Admin 面板展示用）
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct AccountError {
    /// 上游 HTTP 状态码；传输层错误（连接失败、超时等）与 Token 刷新失败为 None
    #[serde(skip_serializing_if = "Option::is_none")]
    pub status_code: Option<u16>,
    pub message: String,
    pub at: DateTime<Utc>,
}

impl AccountError {
    /// 报错原文的最大字符数（上游错误体可能很长，面板只需看清原因）
    const MAX_MESSAGE_CHARS: usize = 500;

    pub(crate) fn new(status_code: Option<u16>, message: &str) -> Self {
        Self {
            status_code,
            message: message.chars().take(Self::MAX_MESSAGE_CHARS).collect(),
            at: Utc::now(),
        }
    }
}

/// 禁用原因
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DisabledReason {
    /// Admin API 手动禁用
    Manual,
    /// 连续 401/403 达到阈值后自动禁用
    TooManyFailures,
    /// 额度已用尽（如 MONTHLY_REQUEST_COUNT）
    QuotaExceeded,
    /// refreshToken 已被服务端永久撤销（invalid_grant），需人工更换凭证
    InvalidRefreshToken,
    /// 企业 IdC 账号缺少 profileArn，上游数据面接口强制要求该字段，需人工补充凭证
    ProfileArnMissing,
    /// Token 刷新连续失败达到阈值后自动禁用
    TooManyRefreshFailures,
}

impl DisabledReason {
    /// 面向客户端的原因描述（用于错误消息，便于从日志直接判断真因）
    pub(crate) fn describe(&self) -> &'static str {
        match self {
            DisabledReason::Manual => "已被手动禁用",
            DisabledReason::TooManyFailures => "因连续 401/403（账号异常）被自动禁用",
            DisabledReason::QuotaExceeded => "本月请求额度已用尽",
            DisabledReason::InvalidRefreshToken => "refreshToken 已被服务端撤销，需人工更换凭证",
            DisabledReason::ProfileArnMissing => {
                "缺少 profileArn，无法发起对话请求，需在账号配置中补充"
            }
            DisabledReason::TooManyRefreshFailures => "因连续 Token 刷新失败被自动禁用",
        }
    }
}

/// 账号健康状态（判定规则见 `MultiTokenManager::compute_health_at`）
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum HealthStatus {
    /// 正常
    Healthy,
    /// 近期有少量上游失败或 1 次认证失败；仅提示
    Warning,
    /// 近期上游失败较多或连续失败；同优先级内最后被选中
    Degraded,
    /// 冷却已结束，等一次完整成功的响应才回到健康；同一时刻只放行一个请求，选号顺位不降档
    PendingVerification,
    /// 冷却期内（限流 / 空响应连续或近期很多，请求发送连续失败，或试探等待未满）；选号时排除
    Unhealthy,
    /// 已禁用（手动或自动）
    Disabled,
}

#[allow(dead_code)]
impl HealthStatus {
    /// 返回前端展示用的颜色标识
    pub fn color(&self) -> &'static str {
        match self {
            HealthStatus::Healthy => "green",
            HealthStatus::Warning => "yellow",
            HealthStatus::Degraded | HealthStatus::PendingVerification => "orange",
            HealthStatus::Unhealthy => "red",
            HealthStatus::Disabled => "gray",
        }
    }

    /// 返回中文标签
    pub fn label(&self) -> &'static str {
        match self {
            HealthStatus::Healthy => "健康",
            HealthStatus::Warning => "警告",
            HealthStatus::Degraded => "降级",
            HealthStatus::PendingVerification => "恢复待验证",
            HealthStatus::Unhealthy => "不健康",
            HealthStatus::Disabled => "已禁用",
        }
    }
}

/// 统计数据持久化条目
#[derive(Serialize, Deserialize)]
pub(crate) struct StatsEntry {
    pub(crate) success_count: u64,
    pub(crate) last_used_at: Option<String>,
    #[serde(default)]
    pub(crate) throttle_count: u64,
    #[serde(default)]
    pub(crate) last_throttled_wall: Option<String>,
    /// 自动禁用原因（仅持久化自动判定的原因；Manual 由 credentials.json 承载）
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub(crate) disabled_reason: Option<DisabledReason>,
    /// 额度用尽时间（UTC RFC3339），用于跨自然月自动恢复
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub(crate) quota_exhausted_at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub(crate) last_error: Option<AccountError>,
}
