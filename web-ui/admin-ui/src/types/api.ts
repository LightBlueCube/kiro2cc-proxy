// Copyright (c) 2026 Harllan He. Licensed under MIT.
// 凭据状态响应
export interface CredentialsStatusResponse {
  total: number
  available: number
  currentId: number
  credentials: CredentialStatusItem[]
}

// 单个凭据状态
export interface CredentialStatusItem {
  id: number
  priority: number
  disabled: boolean
  failureCount: number
  isCurrent: boolean
  expiresAt: string | null
  authMethod: string | null
  hasProfileArn: boolean
  /** OIDC Client ID（IdC / external_idp；非机密，编辑弹窗只读展示） */
  clientId?: string
  /** Profile ARN（非机密，编辑弹窗只读展示） */
  profileArn?: string
  /** 账号级 Machine ID（非机密，编辑弹窗只读展示） */
  machineId?: string
  email?: string
  nickname?: string
  /** 登录来源 / IdP 标识（social 账号为 Google / GitHub；external_idp 为 AzureAD 等） */
  provider?: string
  /** 账号级 Auth Region（未配置时回退全局） */
  authRegion?: string
  /** 账号级 API Region（未配置时回退全局） */
  apiRegion?: string
  refreshTokenHash?: string
  successCount: number
  lastUsedAt: string | null
  hasProxy: boolean
  proxyUrl?: string
  healthStatus: 'healthy' | 'warning' | 'degraded' | 'unhealthy' | 'disabled'
  /** 健康度判定依据：最近 10 分钟上游限流 / 异常结束（空响应、上游中止、thinking 后截断）次数、连续上游失败次数 */
  recentThrottleCount: number
  recentAbnormalCount: number
  consecutiveFailures: number
  /** 不健康时冷却期剩余秒数（冷却期内选号排除），其它状态缺省 */
  cooldownRemainingSecs?: number
  /** 请求发送连续失败（代理错误、连接失败、超时）的冷却剩余秒数；显示为异常，到期自动重试 */
  unreachableCooldownSecs?: number
  throttleCount: number
  /**
   * 禁用原因；与 `disabled` 配套使用。
   * - quota_exceeded：上游 402 MONTHLY_REQUEST_COUNT 触发，跨自然月自动恢复
   * - 其他：手动 / 连续失败 / refreshToken 撤销 等需人工介入
   */
  disabledReason?:
    | 'manual'
    | 'too_many_failures'
    | 'quota_exceeded'
    | 'invalid_refresh_token'
    | 'too_many_refresh_failures'
    | 'profile_arn_missing'
  /** 本轮连续失败中最近一次账号报错（请求失败或 Token 刷新失败）；正常完成一次响应后清除 */
  lastError?: AccountError
  /** 因连续拒答处于冷却期的模型（期间该模型的请求换用其它账号）；无限制时缺省 */
  modelRestrictions?: ModelRestriction[]
}

export interface ModelRestriction {
  /** 模型 ID（小写） */
  model: string
  remainingSecs: number
  /** 最近一次拒答的上游说明原文 */
  message: string
}

export interface AccountError {
  /** 上游 HTTP 状态码；连接失败、超时等传输层错误与 Token 刷新失败时缺省 */
  statusCode?: number
  message: string
  /** RFC3339 */
  at: string
}

// 余额响应
export interface BalanceResponse {
  id: number
  subscriptionTitle: string | null
  currentUsage: number
  usageLimit: number
  remaining: number
  usagePercentage: number
  nextResetAt: number | null
}

// 成功响应
export interface SuccessResponse {
  success: boolean
  message: string
}

// 错误响应
export interface AdminErrorResponse {
  error: {
    type: string
    message: string
  }
}

// 请求类型
export interface SetDisabledRequest {
  disabled: boolean
}

export interface SetPriorityRequest {
  priority: number
}

// 添加凭据请求
export interface AddCredentialRequest {
  refreshToken: string
  authMethod?: 'social' | 'idc'
  email?: string
  nickname?: string
  clientId?: string
  clientSecret?: string
  profileArn?: string
  priority?: number
  disabled?: boolean
  authRegion?: string
  apiRegion?: string
  machineId?: string
  proxyUrl?: string
  proxyUsername?: string
  proxyPassword?: string
  /** 登录来源 / IdP 标识（social 账号：Google / GitHub） */
  provider?: string
}

// 更新凭据请求
export interface UpdateCredentialRequest {
  refreshToken?: string
  authMethod?: string
  email?: string
  nickname?: string
  clientId?: string
  clientSecret?: string
  profileArn?: string
  authRegion?: string
  apiRegion?: string
  machineId?: string
  proxyUrl?: string
  proxyUsername?: string
  proxyPassword?: string
  /** 登录来源 / IdP 标识（social 账号：Google / GitHub；空串清除该字段） */
  provider?: string
}

// 添加凭据响应
export interface AddCredentialResponse {
  success: boolean
  message: string
  credentialId: number
  email?: string
}

// API Key 类型
export interface ApiKeyItem {
  id: number
  key: string
  name: string
  enabled: boolean
  createdAt: string
  expiresAt: string | null
  spendingLimit: number | null
  limitUnit: 'usd' | 'credits'
  durationDays: number | null
  activatedAt: string | null
  boundCredentialIds?: number[]
  /** 可调用模型白名单；缺省表示不限制 */
  allowedModels?: string[]
  /** 每分钟请求数上限，0 = 不限 */
  rpmLimit: number
  /** 同时在途请求数上限，0 = 不限 */
  concurrencyLimit: number
}

export interface CreateApiKeyRequest {
  name: string
  expiresAt?: string | null
  spendingLimit?: number | null
  limitUnit?: 'usd' | 'credits'
  durationDays?: number | null
  boundCredentialIds?: number[] | null
  allowedModels?: string[] | null
  rpmLimit?: number
  concurrencyLimit?: number
}

export interface UpdateApiKeyRequest {
  name?: string
  enabled?: boolean
  expiresAt?: string | null
  spendingLimit?: number | null
  limitUnit?: 'usd' | 'credits'
  durationDays?: number | null
  boundCredentialIds?: number[] | null
  allowedModels?: string[] | null
  rpmLimit?: number
  concurrencyLimit?: number
}

// API Key 用量汇总
export interface UsageSummary {
  apiKeyId: number
  totalRequests: number
  totalInputTokens: number
  totalOutputTokens: number
  /** 缓存命中的输入 tokens（已含于 totalInputTokens） */
  totalCacheReadTokens: number
  totalCost: number
  totalCredits: number
  totalCreditsSaved?: number
  byModel: ModelUsage[]
}

export interface ModelUsage {
  model: string
  requests: number
  inputTokens: number
  outputTokens: number
  /** 缓存命中的输入 tokens（已含于 inputTokens） */
  cacheReadTokens: number
  cost: number
  credits: number
  creditsSaved: number
}

// 支持的模型条目（字段名与后端 Model 结构体保持一致，非 camelCase）
export interface ModelItem {
  id: string
  object: string
  created: number
  owned_by: string
  display_name: string
  type: string
  max_tokens: number
  rate_multiplier?: number | null
  /** 模型发布日期（YYYY-MM-DD）；未登记为 null，前端据此实时计算「最近新增」标记 */
  released_at?: string | null
  /** 官方模型描述；上游未返回或回退静态表时为 null */
  description?: string | null
}

export interface ModelsResponse {
  object: string
  data: ModelItem[]
}

// 更新日志（字段名与后端 AdminReleaseNote 结构体保持一致，非 camelCase）
export interface BilingualText {
  zh: string
  en: string
}

export interface ReleaseNoteGroup {
  title_zh: string
  title_en: string
  items: BilingualText[]
}

export interface ReleaseNote {
  version: string
  is_latest: boolean
  groups: ReleaseNoteGroup[]
}

export interface ReleaseNotesResponse {
  object: string
  data: ReleaseNote[]
}

// RPM 与并发实时监控（并发 = 正在处理的请求数；无在途请求的键不出现）
export interface RpmSnapshot {
  global: number
  byCredential: Record<string, number>
  byApiKey: Record<string, number>
  concurrencyGlobal: number
  concurrencyByCredential: Record<string, number>
  concurrencyByApiKey: Record<string, number>
  /** 启动以来选号落在会话绑定账号的次数（未启用亲和时缺省） */
  affinityPreferred?: number
  /** 启动以来有效绑定被改到其它账号的次数 */
  affinityRebinds?: number
  /** 启动以来无绑定会话按前缀族落到已缓存该前缀账号的次数 */
  affinityPrefixHits?: number
}

// 单条原始请求记录
export interface UsageRecord {
  model: string
  inputTokens: number
  outputTokens: number
  estimatedCost: number
  creditsUsed?: number
  creditsSaved?: number
  cacheReadInputTokens?: number
  cacheCreationInputTokens?: number
  createdAt: string
  apiKeyId: number
  apiKeyName?: string
  credentialId?: number
  credentialLabel?: string
  clientIp?: string
  effort?: string
  thinkingBudgetTokens?: number
  firstTokenMs?: number
  durationMs?: number
  thinkingTokenCount?: number
  isStream?: boolean
  userAgent?: string
  thinkingLoss?: ThinkingLoss
  apiFormat?: ApiFormat
  /** 请求以错误结束（出错的请求只在上游计费时才记录）；正常完成与旧记录缺省 */
  error?: UsageError
}

export type UsageError =
  | 'refused'
  | 'aborted'
  | 'thinking_only_truncated'
  | 'empty'
  | 'empty_oversized_context'
  | 'interrupted'

// 客户端请求所用的 API 格式（按入口端点区分；旧记录缺省）
export type ApiFormat = 'anthropic-messages' | 'cc-messages' | 'openai-chat' | 'openai-responses'

// 历史推理未能回传上游的情况（字段缺省即 0 / false）
export interface ThinkingLoss {
  unsignedTurns?: number
  renderedTurns?: number
  invalidTurns?: number
  signatureRejected?: boolean
}

// 分页原始记录响应
export interface UsageRecordsResponse {
  records: UsageRecord[]
  total: number
  page: number
  pageSize: number
  totalPages: number
}

// 每日用量汇总
export interface DailySummary {
  date: string          // "2026-05-21" CST (UTC+8)，与 get_daily_summaries() 的聚合口径一致
  totalRequests: number
  totalCost: number
  totalCredits: number
  totalCreditsSaved?: number
  totalInputTokens: number      // 累计输入 tokens（含 cache 命中部分）
  totalOutputTokens: number
  totalCacheReadTokens: number  // 累计 cache 命中的输入 tokens
}

// 单账号全部留存记录的用量汇总（后端 /credentials/:id/usage/summary，不限日期）
export interface CredentialUsageSummary {
  credentialId: number
  totalRequests: number
  totalInputTokens: number
  totalOutputTokens: number
  totalCost: number
  totalCredits: number
  totalCreditsSaved?: number
  totalCacheReadTokens: number
}

// 单账号在某 CST 日期的用量汇总（后端 /credentials/:id/usage/today）
export interface CredentialDaySummary {
  date: string           // "2026-06-26" CST (UTC+8)
  credentialId: number
  totalRequests: number
  totalInputTokens: number
  totalOutputTokens: number
  totalCost: number
  totalCredits: number
  totalCreditsSaved?: number
}

// 单账号在 CST 最近 N 天（含今日）的用量汇总（后端 /credentials/:id/usage/7days、/30days）
export interface CredentialRangeSummary {
  credentialId: number
  fromDate: string       // "2026-10-16" CST，区间起始日（含）
  toDate: string         // 区间结束日（含），恒为今日
  totalRequests: number
  totalInputTokens: number
  totalOutputTokens: number
  totalCost: number
  totalCredits: number
  totalCreditsSaved?: number
}

// 单条限流日志记录
export interface ThrottleLogRecord {
  credentialId: number
  requestType: string
  statusCode: number
  responseBody: string
  createdAt: string
  // 以下为限流现场，旧记录没有
  endpoint?: string
  /** 该账号最近 60 秒成功请求数 */
  credentialRpm?: number
  /** 该账号正在生成中的响应数 */
  credentialInFlight?: number
  /** 本次请求内第几次尝试（从 1 起） */
  attempt?: number
  model?: string
}

// 限流日志分页响应
export interface ThrottleLogsResponse {
  records: ThrottleLogRecord[]
  total: number
  page: number
  pageSize: number
  totalPages: number
}

// 单条失败日志记录
export interface FailureLogRecord {
  credentialId: number
  requestType: string
  statusCode: number
  responseBody: string
  createdAt: string
}

// 失败日志分页响应
export interface FailureLogsResponse {
  records: FailureLogRecord[]
  total: number
  page: number
  pageSize: number
  totalPages: number
}
