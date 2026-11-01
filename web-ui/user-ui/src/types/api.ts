// Copyright (c) 2026 Harllan He. Licensed under MIT.
export interface LoginRequest {
  apiKey: string
}

export interface LoginResponse {
  id: number
  name: string
  spendingLimit: number | null
  limitUnit: 'usd' | 'credits'
  totalCost: number
  totalCredits: number
  expiresAt: string | null
  durationDays: number | null
  activatedAt: string | null
}

export interface ModelUsage {
  model: string
  requests: number
  inputTokens: number
  outputTokens: number
  /** 缓存命中的输入 tokens（已含于 inputTokens） */
  cacheReadTokens: number
  cost: number
}

export interface UsageResponse {
  id: number
  name: string
  spendingLimit: number | null
  limitUnit: 'usd' | 'credits'
  expiresAt: string | null
  durationDays: number | null
  activatedAt: string | null
  totalRequests: number
  totalInputTokens: number
  totalOutputTokens: number
  /** 缓存命中的输入 tokens（已含于 totalInputTokens） */
  totalCacheReadTokens: number
  totalCost: number
  totalCredits: number
  byModel: ModelUsage[]
}

export interface UsageRecordItem {
  model: string
  inputTokens: number
  outputTokens: number
  estimatedCost: number
  creditsUsed?: number
  creditsSaved?: number
  cacheReadInputTokens?: number
  cacheCreationInputTokens?: number
  createdAt: string
  clientIp?: string
  credentialLabel?: string
  thinkingTokenCount?: number
  isStream?: boolean
  userAgent?: string
  thinkingLoss?: ThinkingLoss
  effort?: string
  thinkingBudgetTokens?: number
  firstTokenMs?: number
  durationMs?: number
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

/** 客户端请求所用的 API 格式（按入口端点区分；旧记录缺省） */
export type ApiFormat = 'anthropic-messages' | 'cc-messages' | 'openai-chat' | 'openai-responses'

/** 历史推理未能回传上游的情况（字段缺省即 0 / false） */
export interface ThinkingLoss {
  unsignedTurns?: number
  renderedTurns?: number
  invalidTurns?: number
  signatureRejected?: boolean
}

export interface UsageRecordsPage {
  records: UsageRecordItem[]
  total: number
  page: number
  pageSize: number
  totalPages: number
}
