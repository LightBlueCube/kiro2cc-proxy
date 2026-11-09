// Copyright (c) 2026 Harllan He. Licensed under MIT.

//! 配置模型：config.json 反序列化结构（TlsBackend、缓存模拟、限流、超时等）与环境变量覆盖逻辑。
use serde::{Deserialize, Serialize};
use std::env;
use std::fs;
use std::path::{Path, PathBuf};

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "kebab-case")]
#[derive(Default)]
pub enum TlsBackend {
    #[default]
    Rustls,
    NativeTls,
}

/// 对话请求走的 Kiro 上游 API（启动时读取，修改后须重启）
///
/// - `createResponse`：`POST /v1/responses`（OpenAI Responses 形态，SSE 响应），Kiro 客户端当前默认
/// - `generateAssistantResponse`：`POST /generateAssistantResponse`（AWS Event Stream 响应）
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq, Default)]
#[serde(rename_all = "camelCase")]
pub enum UpstreamApi {
    #[default]
    CreateResponse,
    GenerateAssistantResponse,
}

impl UpstreamApi {
    /// 配置值（与 serde 表示一致）
    pub fn as_str(self) -> &'static str {
        match self {
            Self::CreateResponse => "createResponse",
            Self::GenerateAssistantResponse => "generateAssistantResponse",
        }
    }
}

impl std::str::FromStr for UpstreamApi {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "createResponse" => Ok(Self::CreateResponse),
            "generateAssistantResponse" => Ok(Self::GenerateAssistantResponse),
            other => Err(format!(
                "upstreamApi 只能是 createResponse 或 generateAssistantResponse，收到 {other:?}"
            )),
        }
    }
}

/// KNA 应用配置
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Config {
    #[serde(default = "default_host")]
    pub host: String,

    #[serde(default = "default_port")]
    pub port: u16,

    #[serde(default = "default_region")]
    pub region: String,

    /// Auth Region（用于 Token 刷新），未配置时回退到 region
    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub auth_region: Option<String>,

    /// API Region（用于 API 请求），未配置时回退到 region
    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub api_region: Option<String>,

    #[serde(default = "default_kiro_version")]
    pub kiro_version: String,

    #[serde(default)]
    pub machine_id: Option<String>,

    #[serde(default = "default_system_version")]
    pub system_version: String,

    #[serde(default = "default_node_version")]
    pub node_version: String,

    #[serde(default = "default_tls_backend")]
    pub tls_backend: TlsBackend,

    /// 对话请求走的 Kiro 上游 API，默认 createResponse；修改后须重启
    #[serde(default)]
    pub upstream_api: UpstreamApi,

    /// 外部 count_tokens API 地址（可选）
    #[serde(default)]
    pub count_tokens_api_url: Option<String>,

    /// count_tokens API 密钥（可选）
    #[serde(default)]
    pub count_tokens_api_key: Option<String>,

    /// count_tokens API 认证类型（可选，"x-api-key" 或 "bearer"，默认 "x-api-key"）
    #[serde(default = "default_count_tokens_auth_type")]
    pub count_tokens_auth_type: String,

    /// HTTP 代理地址（可选）
    /// 支持格式: http://host:port, https://host:port, socks5://host:port
    #[serde(default)]
    pub proxy_url: Option<String>,

    /// 代理认证用户名（可选）
    #[serde(default)]
    pub proxy_username: Option<String>,

    /// 代理认证密码（可选）
    #[serde(default)]
    pub proxy_password: Option<String>,

    /// Admin Password（可选，启用 Admin API 功能）
    #[serde(default, alias = "adminApiKey")]
    pub admin_psw: Option<String>,

    /// 负载均衡模式（"priority" 或 "balanced"）
    #[serde(default = "default_load_balancing_mode")]
    pub load_balancing_mode: String,

    /// 单账号每分钟最大请求数（硬上限：满了换号，无号可换时排队，仍满返回 429），0 表示不限制
    #[serde(default = "default_max_rpm_per_credential")]
    pub max_rpm_per_credential: u32,

    /// 单账号 RPM 软上限：达到后新会话不再绑定到该账号（已绑定的会话不受影响），0 表示关闭
    #[serde(default = "default_soft_rpm_per_credential")]
    pub soft_rpm_per_credential: u32,

    /// 优先级模式下，最高一档账号全部达到软上限时，新会话是否落到下一档账号
    #[serde(default = "default_true")]
    pub soft_rpm_spill_to_lower_priority: bool,

    /// 单账号同时在途请求数上限（从发出到响应体发完，含流式生成；超出时排队等待），0 表示不限制
    #[serde(default)]
    pub max_concurrency_per_credential: u32,

    /// `/v1/models` 动态列表缓存 TTL（秒），默认 3600
    #[serde(default = "default_model_cache_ttl_secs")]
    pub model_cache_ttl_secs: u64,

    /// 客户端 token 直通（true 时 usage 字段 1:1 上报真实值，不再按
    /// CLIENT_TOKEN_DISPLAY_SCALE 缩放）。默认 false 维持既有展示缩放。
    #[serde(default)]
    pub client_token_passthrough: bool,

    /// `/v1/models` 是否为点号模型 ID 追加官方连字符别名（如 `claude-opus-4.6` →
    /// `claude-opus-4-6`）。供按模型列表生成映射的下游网关使用；默认 false，
    /// 避免 Claude Code 模型选择器出现重复项
    #[serde(default)]
    pub models_official_aliases: bool,

    /// 对客户端上报的 output_tokens 是否包含 thinking（Anthropic 官方口径为包含）。
    /// 默认 false 保持既有行为（仅可见输出）。
    #[serde(default)]
    pub output_tokens_include_thinking: bool,

    /// Suggestion Mode 输入建议请求放行开关
    ///
    /// Claude Code 每轮主对话后会自动发起输入建议请求（末条消息以 `[SUGGESTION MODE`
    /// 开头），携带全量上下文却只产出候选短语，转发上游按全价计费。
    /// 默认 false：代理拦截并返回空响应（不转发、不计费）；
    /// true：正常转发 Kiro 上游（保留客户端输入框建议短语功能）。
    #[serde(default)]
    pub forward_suggestion_mode: bool,

    /// 思考内容文本化展示（仅 Claude Code 客户端）
    ///
    /// Claude Code 默认隐藏/折叠 thinking，长推理期间界面像卡住。开启后，出站 SSE 的
    /// thinking 块被改写为 text 块逐行流式输出（类似 Kiro CLI 的展示），样式按客户端入口选择：
    /// 终端入口（`cli`）用 ANSI 变暗斜体（灰色）；Claude Desktop、`local-agent` 等其他入口不解析
    /// ANSI，改用无转义的 Markdown 引用块 + 斜体（每行 `> *内容*`）近似变暗。
    /// 回传的历史里这些文本会被自动剥离，上游看不到；代理上报的 input_tokens 不含它们，output_tokens 默认也不含（见 outputTokensIncludeThinking）。
    /// 默认 false。
    /// 代价：改写后客户端拿不到推理签名，历史轮次的原始思维链无法回传上游，后续轮次的模型
    /// 看不到自己之前的推理（文本化的思考摘要回传时同样被剥离）；面板使用日志会标出这类轮次。
    /// 副作用：客户端本地的消息列表仍包含这些文本，其本地估算与自身 compact 的输入会相应变长。
    #[serde(default)]
    pub thinking_as_text: bool,

    /// 分块写入约束的行数上限（issue #46）
    ///
    /// 请求带 Write / Edit / MultiEdit 工具时，代理在系统提示末尾追加“单次工具调用参数不超过
    /// N 行”的约束，避免上游对大块工具参数静默约 240s 后重置流。约束会让模型把大写入拆成
    /// 多轮，每轮重新思考，credits 随之上升。默认 300；调大更省 credits 但更容易撞 240s 重置；
    /// 0 表示不追加约束。可在 Admin 设置页热修改（仅影响之后的新请求）。
    #[serde(default = "default_write_chunking_max_lines")]
    pub write_chunking_max_lines: u32,

    /// Kiro 请求体落盘取证开关（临时调试用）
    ///
    /// true 时每个发往 Kiro 的请求体都会写入 `app/config/kiro-body-dump/`
    /// （0600 权限，文件名含毫秒时间戳），用于排查协议转换问题。
    /// 仍兼容环境变量 `KIRO_DUMP_BODY=1`（两者任一满足即生效）。
    /// 默认 false：不落盘。定位问题后应及时关闭。
    #[serde(default)]
    pub dump_kiro_body: bool,

    /// 日志文件路径（可选）
    ///
    /// 配置后日志除控制台外同步以 append 模式写入该文件，便于持久化排查。
    /// 文件打开失败时仅告警降级（只输出控制台），不影响启动。
    /// 未配置时行为与现状完全一致（仅控制台）。
    /// 注意：文件持续追加写入，无自动轮转，长期运行请配合外部 logrotate 或定期清理。
    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub log_file: Option<String>,

    /// 是否在日志中输出 DEBUG 级别事件（控制台 / 日志文件 / Admin 日志页三处一致）
    ///
    /// 默认 false 保持 info 级过滤。可在 Admin 设置页热切换并写回 config.json；
    /// 开启时在 RUST_LOG 基础上追加全局 debug 指令，target 定向指令仍优先。
    #[serde(default)]
    pub debug_logs: bool,

    /// 配置文件路径（运行时元数据，不写入 JSON）
    #[serde(skip)]
    config_path: Option<PathBuf>,

    /// 读到的已废弃配置项与环境变量（运行时元数据：加载时日志尚未初始化，由启动流程统一告警）
    #[serde(skip)]
    removed_settings: Vec<&'static str>,
}

fn default_write_chunking_max_lines() -> u32 {
    crate::anthropic::DEFAULT_WRITE_CHUNKING_MAX_LINES
}

fn default_host() -> String {
    "127.0.0.1".to_string()
}

fn default_port() -> u16 {
    8080
}

fn default_region() -> String {
    "us-east-1".to_string()
}

fn default_kiro_version() -> String {
    "2.2.2".to_string()
}

fn default_max_rpm_per_credential() -> u32 {
    30
}

fn default_soft_rpm_per_credential() -> u32 {
    10
}

fn default_true() -> bool {
    true
}

fn default_system_version() -> String {
    "darwin#24.6.0".to_string()
}

fn default_node_version() -> String {
    "22.21.1".to_string()
}

fn default_count_tokens_auth_type() -> String {
    "x-api-key".to_string()
}

fn default_tls_backend() -> TlsBackend {
    TlsBackend::Rustls
}

fn default_load_balancing_mode() -> String {
    "priority".to_string()
}

fn default_model_cache_ttl_secs() -> u64 {
    3600
}

impl Default for Config {
    fn default() -> Self {
        Self {
            host: default_host(),
            port: default_port(),
            region: default_region(),
            auth_region: None,
            api_region: None,
            kiro_version: default_kiro_version(),
            machine_id: None,
            system_version: default_system_version(),
            node_version: default_node_version(),
            tls_backend: default_tls_backend(),
            upstream_api: UpstreamApi::default(),
            count_tokens_api_url: None,
            count_tokens_api_key: None,
            count_tokens_auth_type: default_count_tokens_auth_type(),
            proxy_url: None,
            proxy_username: None,
            proxy_password: None,
            admin_psw: None,
            load_balancing_mode: default_load_balancing_mode(),
            max_rpm_per_credential: default_max_rpm_per_credential(),
            soft_rpm_per_credential: default_soft_rpm_per_credential(),
            soft_rpm_spill_to_lower_priority: true,
            max_concurrency_per_credential: 0,
            model_cache_ttl_secs: default_model_cache_ttl_secs(),
            client_token_passthrough: false,
            models_official_aliases: false,
            output_tokens_include_thinking: false,
            forward_suggestion_mode: false,
            thinking_as_text: false,
            write_chunking_max_lines: default_write_chunking_max_lines(),
            dump_kiro_body: false,
            log_file: None,
            debug_logs: false,
            config_path: None,
            removed_settings: Vec::new(),
        }
    }
}

impl Config {
    /// 获取默认配置文件路径
    pub fn default_config_path() -> &'static str {
        "config.json"
    }

    /// 获取有效的 Auth Region（用于 Token 刷新）
    /// 优先使用 auth_region，未配置时回退到 region
    pub fn effective_auth_region(&self) -> &str {
        self.auth_region.as_deref().unwrap_or(&self.region)
    }

    /// 获取有效的 API Region（用于 API 请求）
    /// 优先使用 api_region，未配置时回退到 region
    pub fn effective_api_region(&self) -> &str {
        self.api_region.as_deref().unwrap_or(&self.region)
    }

    /// 从文件加载配置
    pub fn load<P: AsRef<Path>>(path: P) -> anyhow::Result<Self> {
        let path = path.as_ref();
        if !path.exists() {
            return Ok(Self {
                config_path: Some(path.to_path_buf()),
                ..Self::default()
            });
        }

        let content = fs::read_to_string(path)?;
        let mut config: Config = serde_json::from_str(&content)?;
        config.removed_settings = removed_keys_in(&content)?;
        config.config_path = Some(path.to_path_buf());
        Ok(config)
    }

    /// 读到的已废弃配置项（config.json 键名）与环境变量名。缓存用量改为按 credits 反推 /
    /// 同账号前缀估算（见 `crate::cache`），这些设置不再生效
    pub fn removed_settings(&self) -> &[&'static str] {
        &self.removed_settings
    }

    /// 获取配置文件路径（如果有）
    pub fn config_path(&self) -> Option<&Path> {
        self.config_path.as_deref()
    }

    /// 从环境变量覆盖配置项（用于容器化部署，如 Zeabur）
    ///
    /// 支持的环境变量:
    /// - `HOST`: 监听地址
    /// - `PORT`: 监听端口
    /// - `REGION`: AWS 区域
    /// - `AUTH_REGION`: Token 刷新区域
    /// - `API_REGION`: API 请求区域
    /// - `ADMIN_PSW`: Admin Password（未设置时回退读取 `ADMIN_API_KEY`）
    /// - `PROXY_URL`: HTTP 代理地址
    /// - `PROXY_USERNAME`: 代理用户名
    /// - `PROXY_PASSWORD`: 代理密码
    /// - `LOAD_BALANCING_MODE`: 负载均衡模式
    /// - `MODEL_CACHE_TTL_SECS`: /v1/models 动态列表缓存 TTL（秒）
    /// - `CLIENT_TOKEN_PASSTHROUGH`: 客户端 token 直通开关
    /// - `WRITE_CHUNKING_MAX_LINES`: 分块写入约束行数上限
    /// - `DUMP_KIRO_BODY`: Kiro 请求体落盘取证开关
    /// - `LOG_FILE`: 日志文件路径
    pub fn apply_env_overrides(&mut self) {
        if let Ok(v) = env::var("HOST") {
            self.host = v;
        }
        if let Ok(v) = env::var("PORT")
            && let Ok(p) = v.parse::<u16>()
        {
            self.port = p;
        }
        if let Ok(v) = env::var("REGION") {
            self.region = v;
        }
        if let Ok(v) = env::var("AUTH_REGION") {
            self.auth_region = Some(v);
        }
        if let Ok(v) = env::var("API_REGION") {
            self.api_region = Some(v);
        }
        if let Ok(v) = env::var("ADMIN_PSW") {
            self.admin_psw = Some(v);
        } else if let Ok(v) = env::var("ADMIN_API_KEY") {
            self.admin_psw = Some(v);
        }
        if let Ok(v) = env::var("PROXY_URL") {
            self.proxy_url = Some(v);
        }
        if let Ok(v) = env::var("PROXY_USERNAME") {
            self.proxy_username = Some(v);
        }
        if let Ok(v) = env::var("PROXY_PASSWORD") {
            self.proxy_password = Some(v);
        }
        if let Ok(v) = env::var("LOAD_BALANCING_MODE") {
            self.load_balancing_mode = v;
        }
        if let Ok(v) = env::var("MODEL_CACHE_TTL_SECS")
            && let Ok(n) = v.parse::<u64>()
        {
            self.model_cache_ttl_secs = n;
        }
        if let Ok(v) = env::var("CLIENT_TOKEN_PASSTHROUGH")
            && let Ok(b) = v.parse::<bool>()
        {
            self.client_token_passthrough = b;
        }
        if let Ok(v) = env::var("MODELS_OFFICIAL_ALIASES")
            && let Ok(b) = v.parse::<bool>()
        {
            self.models_official_aliases = b;
        }
        if let Ok(v) = env::var("OUTPUT_TOKENS_INCLUDE_THINKING")
            && let Ok(b) = v.parse::<bool>()
        {
            self.output_tokens_include_thinking = b;
        }
        if let Ok(v) = env::var("FORWARD_SUGGESTION_MODE")
            && let Ok(b) = v.parse::<bool>()
        {
            self.forward_suggestion_mode = b;
        }
        if let Ok(v) = env::var("DUMP_KIRO_BODY")
            && let Ok(b) = v.parse::<bool>()
        {
            self.dump_kiro_body = b;
        }
        if let Ok(v) = env::var("LOG_FILE")
            && !v.trim().is_empty()
        {
            self.log_file = Some(v);
        }
        if let Ok(v) = env::var("DEBUG_LOGS")
            && let Ok(b) = v.parse::<bool>()
        {
            self.debug_logs = b;
        }
        if let Ok(v) = env::var("THINKING_AS_TEXT")
            && let Ok(b) = v.parse::<bool>()
        {
            self.thinking_as_text = b;
        }
        if let Ok(v) = env::var("WRITE_CHUNKING_MAX_LINES")
            && let Ok(n) = v.parse::<u32>()
        {
            self.write_chunking_max_lines = n;
        }
        if let Ok(v) = env::var("UPSTREAM_API")
            && let Ok(api) = v.parse::<UpstreamApi>()
        {
            self.upstream_api = api;
        }

        self.removed_settings.extend(
            REMOVED_ENV_VARS
                .into_iter()
                .filter(|name| env::var_os(name).is_some()),
        );
    }
}

/// 已废弃的缓存模拟配置项（config.json 键名）。读到时只告警，不影响启动；
/// 下次保存配置时自然移除
const REMOVED_KEYS: [&str; 3] = [
    "cacheSimulation",
    "promptCacheAccounting",
    "promptCacheMinTokens",
];

/// 已废弃的缓存模拟环境变量（同 [`REMOVED_KEYS`]）
const REMOVED_ENV_VARS: [&str; 7] = [
    "PROMPT_CACHE_ACCOUNTING",
    "PROMPT_CACHE_MIN_TOKENS",
    "CACHE_SIMULATION_FINGERPRINT_ENABLED",
    "CACHE_SIMULATION_FINGERPRINT_TTL_5M",
    "CACHE_SIMULATION_FINGERPRINT_TTL_1H",
    "CACHE_SIMULATION_EPHEMERAL_1H_RATIO",
    "CACHE_SIMULATION_FINGERPRINT_MAX_BREAKPOINTS",
];

/// config.json 中仍带着的废弃键
fn removed_keys_in(content: &str) -> serde_json::Result<Vec<&'static str>> {
    let value: serde_json::Value = serde_json::from_str(content)?;
    Ok(REMOVED_KEYS
        .into_iter()
        .filter(|key| value.get(key).is_some())
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rpm_limits_default_to_soft_10_hard_30_with_spill() {
        let config: Config = serde_json::from_str("{}").unwrap();
        assert_eq!(config.max_rpm_per_credential, 30);
        assert_eq!(config.soft_rpm_per_credential, 10);
        assert!(config.soft_rpm_spill_to_lower_priority);
        assert_eq!(Config::default().max_rpm_per_credential, 30);
    }

    #[test]
    fn removed_cache_keys_are_detected_and_ignored() {
        let content = r#"{"promptCacheAccounting": true, "cacheSimulation": {}, "port": 1}"#;
        let config: Config = serde_json::from_str(content).unwrap();
        assert_eq!(config.port, 1);
        assert_eq!(
            removed_keys_in(content).unwrap(),
            ["cacheSimulation", "promptCacheAccounting"]
        );
    }

    #[test]
    fn upstream_api_defaults_to_create_response_and_round_trips() {
        assert_eq!(Config::default().upstream_api, UpstreamApi::CreateResponse);
        let c: Config = serde_json::from_str("{}").unwrap();
        assert_eq!(c.upstream_api, UpstreamApi::CreateResponse);

        let c: Config =
            serde_json::from_str(r#"{"upstreamApi": "generateAssistantResponse"}"#).unwrap();
        assert_eq!(c.upstream_api, UpstreamApi::GenerateAssistantResponse);
        assert!(serde_json::from_str::<Config>(r#"{"upstreamApi": "sendMessage"}"#).is_err());

        for api in [
            UpstreamApi::CreateResponse,
            UpstreamApi::GenerateAssistantResponse,
        ] {
            assert_eq!(api.as_str().parse::<UpstreamApi>(), Ok(api));
            assert_eq!(
                serde_json::to_string(&api).unwrap(),
                format!("\"{}\"", api.as_str())
            );
        }
        assert!("CreateResponse".parse::<UpstreamApi>().is_err());
    }

    #[test]
    fn test_model_cache_ttl_default() {
        let config = Config::default();
        assert_eq!(config.model_cache_ttl_secs, 3600);
    }

    #[test]
    fn test_model_cache_ttl_deserialize_default() {
        // 配置缺省该字段时应回退默认 3600
        let config: Config = serde_json::from_str("{}").unwrap();
        assert_eq!(config.model_cache_ttl_secs, 3600);
    }

    #[test]
    fn test_model_cache_ttl_deserialize_explicit() {
        let config: Config = serde_json::from_str(r#"{"modelCacheTtlSecs": 60}"#).unwrap();
        assert_eq!(config.model_cache_ttl_secs, 60);
    }

    #[test]
    fn test_client_token_passthrough_default_false() {
        // 缺省该字段时必须为 false（维持展示缩放，Issue #44 修复的默认零回归前提）
        let config: Config = serde_json::from_str("{}").unwrap();
        assert!(!config.client_token_passthrough);
    }

    #[test]
    fn test_write_chunking_max_lines_default_and_override() {
        assert_eq!(Config::default().write_chunking_max_lines, 300);
        let c: Config = serde_json::from_str("{}").unwrap();
        assert_eq!(c.write_chunking_max_lines, 300);
        let c: Config = serde_json::from_str(r#"{"writeChunkingMaxLines": 0}"#).unwrap();
        assert_eq!(c.write_chunking_max_lines, 0);
        let c: Config = serde_json::from_str(r#"{"writeChunkingMaxLines": 450}"#).unwrap();
        assert_eq!(c.write_chunking_max_lines, 450);
    }

    #[test]
    fn test_thinking_as_text_default_is_false() {
        let config = Config::default();
        assert!(!config.thinking_as_text);
    }

    #[test]
    fn test_thinking_as_text_deserialize_default_is_false() {
        // 缺省该字段时必须为 false：文本化会丢掉推理签名，后续轮次看不到之前的推理
        let config: Config = serde_json::from_str("{}").unwrap();
        assert!(!config.thinking_as_text);
    }

    #[test]
    fn test_client_token_passthrough_deserialize_explicit() {
        let config: Config = serde_json::from_str(r#"{"clientTokenPassthrough": true}"#).unwrap();
        assert!(config.client_token_passthrough);
    }

    #[test]
    fn test_forward_suggestion_mode_default_false() {
        // 缺省该字段时必须为 false（拦截 Suggestion Mode，维持零计费默认行为）
        let config: Config = serde_json::from_str("{}").unwrap();
        assert!(!config.forward_suggestion_mode);
    }

    #[test]
    fn test_forward_suggestion_mode_deserialize_explicit() {
        let config: Config = serde_json::from_str(r#"{"forwardSuggestionMode": true}"#).unwrap();
        assert!(config.forward_suggestion_mode);
    }

    #[test]
    fn test_dump_kiro_body_default_false() {
        // 缺省该字段时必须为 false（默认不落盘请求体）
        let config: Config = serde_json::from_str("{}").unwrap();
        assert!(!config.dump_kiro_body);
    }

    #[test]
    fn test_dump_kiro_body_deserialize_explicit() {
        let config: Config = serde_json::from_str(r#"{"dumpKiroBody": true}"#).unwrap();
        assert!(config.dump_kiro_body);
    }

    #[test]
    fn test_log_file_default_none() {
        // 缺省该字段时必须为 None（仅控制台输出，行为与现状一致）
        let config: Config = serde_json::from_str("{}").unwrap();
        assert!(config.log_file.is_none());
    }

    #[test]
    fn test_log_file_deserialize_explicit() {
        let config: Config =
            serde_json::from_str(r#"{"logFile": "app/config/proxy.log"}"#).unwrap();
        assert_eq!(config.log_file.as_deref(), Some("app/config/proxy.log"));
    }

    #[test]
    fn test_debug_logs_default_false() {
        // 缺省该字段时必须为 false（保持 info 级过滤，行为与现状一致）
        let config: Config = serde_json::from_str("{}").unwrap();
        assert!(!config.debug_logs);
        assert!(!Config::default().debug_logs);
    }

    #[test]
    fn test_debug_logs_deserialize_explicit() {
        let config: Config = serde_json::from_str(r#"{"debugLogs": true}"#).unwrap();
        assert!(config.debug_logs);
        let config: Config = serde_json::from_str(r#"{"debugLogs": false}"#).unwrap();
        assert!(!config.debug_logs);
    }

    #[test]
    fn test_log_file_env_override() {
        // 环境变量覆盖 config.json（容器化部署场景），子进程隔离规避 set_var UB
        const CHILD_MARKER: &str = "KIRO_CONFIG_ENV_TEST_CHILD";
        if env::var_os(CHILD_MARKER).is_some() {
            let mut config: Config = serde_json::from_str("{}").unwrap();
            config.apply_env_overrides();
            assert_eq!(config.log_file.as_deref(), Some("/tmp/proxy-test.log"));
            return;
        }

        let status = std::process::Command::new(env::current_exe().unwrap())
            .args([
                "--exact",
                "model::config::tests::test_log_file_env_override",
                "--test-threads=1",
            ])
            .env(CHILD_MARKER, "1")
            .env("LOG_FILE", "/tmp/proxy-test.log")
            .status()
            .unwrap();
        assert!(status.success());
    }

    #[test]
    fn test_dump_kiro_body_env_override() {
        // 环境变量覆盖 config.json（容器化部署场景），子进程隔离规避 set_var UB
        const CHILD_MARKER: &str = "KIRO_CONFIG_ENV_TEST_CHILD";
        if env::var_os(CHILD_MARKER).is_some() {
            let mut config: Config = serde_json::from_str("{}").unwrap();
            config.apply_env_overrides();
            assert!(config.dump_kiro_body);
            return;
        }

        let status = std::process::Command::new(env::current_exe().unwrap())
            .args([
                "--exact",
                "model::config::tests::test_dump_kiro_body_env_override",
                "--test-threads=1",
            ])
            .env(CHILD_MARKER, "1")
            .env("DUMP_KIRO_BODY", "true")
            .status()
            .unwrap();
        assert!(status.success());
    }

    #[test]
    fn test_client_token_passthrough_env_override() {
        // 环境变量覆盖 config.json（容器化部署场景）。
        // 通过子进程注入环境变量验证：cargo test 默认并行运行其他测试，
        // 在测试进程内 set_var/remove_var（Rust 2024 unsafe）与系统库并发
        // 读取环境存在 UB 风险，子进程隔离可完全规避。
        const CHILD_MARKER: &str = "KIRO_CONFIG_ENV_TEST_CHILD";
        if env::var_os(CHILD_MARKER).is_some() {
            // 子进程分支：父进程已注入 CLIENT_TOKEN_PASSTHROUGH
            let mut config: Config = serde_json::from_str("{}").unwrap();
            config.apply_env_overrides();
            assert!(config.client_token_passthrough);
            return;
        }

        // 父进程分支：以子进程模式重启自身测试二进制
        let status = std::process::Command::new(env::current_exe().unwrap())
            .args([
                "--exact",
                "model::config::tests::test_client_token_passthrough_env_override",
                "--test-threads=1",
            ])
            .env(CHILD_MARKER, "1")
            .env("CLIENT_TOKEN_PASSTHROUGH", "true")
            .status()
            .unwrap();
        assert!(status.success());
    }

    #[test]
    fn test_forward_suggestion_mode_env_override() {
        // 环境变量覆盖 config.json（容器化部署场景），子进程隔离规避 set_var UB
        const CHILD_MARKER: &str = "KIRO_CONFIG_ENV_TEST_CHILD";
        if env::var_os(CHILD_MARKER).is_some() {
            let mut config: Config = serde_json::from_str("{}").unwrap();
            config.apply_env_overrides();
            assert!(config.forward_suggestion_mode);
            return;
        }

        let status = std::process::Command::new(env::current_exe().unwrap())
            .args([
                "--exact",
                "model::config::tests::test_forward_suggestion_mode_env_override",
                "--test-threads=1",
            ])
            .env(CHILD_MARKER, "1")
            .env("FORWARD_SUGGESTION_MODE", "true")
            .status()
            .unwrap();
        assert!(status.success());
    }
}
