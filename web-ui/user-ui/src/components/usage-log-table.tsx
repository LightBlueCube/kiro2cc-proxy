// Copyright (c) 2026 Harllan He. Licensed under MIT.
import { Brain, DatabaseZap } from 'lucide-react'
import { CELL, TH_BASE } from '@/components/table-kit'
import type { GeoInfo } from '@/hooks/use-ip-geo'
import type { ApiFormat, ThinkingLoss, UsageError, UsageRecordItem } from '@/types/api'

/**
 * 请求日志表：与 admin-ui `usage-log-table.tsx` 同构（user-ui 为独立 app，不跨包共享）。
 *
 * 差异：用户侧接口不下发账号与 API Key 名称，原「账号」列改为「客户端」（UA + API 格式）；
 * user-ui 无 i18n，文案为中文。`.panel` 外壳、滚动容器与分页由调用方给出。
 */

/** 模型色调：按名称映射到令牌色，未知模型走 ink-2 */
const MODEL_TONE: Record<string, string> = {
  opus: 'text-brand',
  sonnet: 'text-ok',
  haiku: 'text-warn',
}

export function getModelTone(model: string): string {
  const lower = model.toLowerCase()
  for (const [key, cls] of Object.entries(MODEL_TONE)) {
    if (lower.includes(key)) return cls
  }
  return 'text-ink-2'
}

export function formatCost(n: number): string {
  return `$${n.toFixed(4)}`
}

export function formatTokens(n: number): string {
  return n.toLocaleString('zh-CN')
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

/** UA 短标识：取首个空格前段（如 claude-cli/2.1.5），完整原文走 title 悬停展示 */
function uaShort(ua: string): string {
  return ua.split(/\s+/)[0] || ua
}

/** API 格式展示名与入口端点 */
const API_FORMATS: Record<ApiFormat, { label: string; endpoint: string }> = {
  'anthropic-messages': { label: 'Anthropic Messages', endpoint: '/v1/messages' },
  'cc-messages': { label: 'Anthropic Messages (CC)', endpoint: '/cc/v1/messages' },
  'openai-chat': { label: 'OpenAI Chat', endpoint: '/v1/chat/completions' },
  'openai-responses': { label: 'OpenAI Responses', endpoint: '/v1/responses' },
}

/**
 * 模型档位换算率，逐字镜像后端 `src/model/usage/pricing.rs::get_k_ref`，
 * 仅用于旧记录缺 `creditsUsed` 时的估算（与 admin-ui 同口径）。
 */
function getKRef(model: string): number {
  const m = model.toLowerCase()
  if (
    m.includes('opus-4-7') ||
    m.includes('opus-4.7') ||
    m.includes('opus-4-8') ||
    m.includes('opus-4.8') ||
    m.includes('opus-5') ||
    m.includes('opus.5')
  ) {
    return 2.36
  }
  if (m.includes('opus-4-5') || m.includes('opus-4.5') || m.includes('opus-4-6') || m.includes('opus-4.6')) {
    return 1.9
  }
  if (m.includes('opus') || m.includes('fable')) {
    return 2.36
  }
  return 1.43
}

/** 单条记录的积分：优先真实值，缺失时按该记录模型的档位估算 */
export function recordCredits(record: UsageRecordItem): number {
  return record.creditsUsed ?? record.estimatedCost * getKRef(record.model)
}

/** 估算缓存命中率（缓存读取 / 输入总计，0–100）；输入为 0 时无意义返回 null */
function cacheHitRate(record: UsageRecordItem): number | null {
  if (!(record.inputTokens > 0)) return null
  return Math.min(100, Math.max(0, ((record.cacheReadInputTokens ?? 0) / record.inputTokens) * 100))
}

/** 毫秒 → 「X.Xs」展示（≥60s 转分秒「Xm XXs」，先取整总秒数避免 60s 进位残留） */
function formatDurationMs(ms: number): string {
  const total = Math.round(ms / 1000)
  if (total >= 60) {
    return `${Math.floor(total / 60)}m ${String(total % 60).padStart(2, '0')}s`
  }
  return `${(ms / 1000).toFixed(1)}s`
}

/** 快慢分档阈值（ms）：首字 ≤1s 快 / ≥5s 慢，耗时 ≤10s 快 / ≥60s 慢 */
const FIRST_TOKEN_FAST_MS = 1_000
const FIRST_TOKEN_SLOW_MS = 5_000
const DURATION_FAST_MS = 10_000
const DURATION_SLOW_MS = 60_000

/** 快/中/慢三档底色 */
function durationTone(ms: number, fastMs: number, slowMs: number): string {
  if (ms <= fastMs) return 'bg-ok-soft text-ok'
  if (ms >= slowMs) return 'bg-warn-soft text-warn'
  return 'bg-surface-3 text-ink-2'
}

/** 估算 TPS 分档阈值（tokens/s）：≥60 快 / ≤20 慢 */
const TPS_FAST = 60
const TPS_SLOW = 20

/** TPS 快/中/慢三档底色（越快越好，与耗时方向相反） */
function tpsTone(tps: number): string {
  if (tps >= TPS_FAST) return 'bg-ok-soft text-ok'
  if (tps <= TPS_SLOW) return 'bg-warn-soft text-warn'
  return 'bg-surface-3 text-ink-2'
}

/** 低于该生成耗时（ms）的 TPS 波动过大，不展示 */
const MIN_TPS_GENERATION_MS = 200

/**
 * 估算输出速度（tokens/s）：输出 tokens ÷ 生成耗时，流式扣除首字等待。
 * 输出 tokens 为本地计数（含推理摘要，不含上游隐藏推理），故为估算值。
 */
function estimatedTps(record: UsageRecordItem): number | null {
  if (record.durationMs == null || !(record.outputTokens > 0)) return null
  const generationMs = record.durationMs - (record.firstTokenMs ?? 0)
  if (generationMs < MIN_TPS_GENERATION_MS) return null
  return record.outputTokens / (generationMs / 1000)
}

/** 徽章胶囊统一样式 */
const BADGE_CLS = 'rounded-[4px] border border-transparent px-1 text-[9.5px] font-semibold'

/** 「用时」单元格：流式双徽章（首字/耗时），非流式仅耗时，末行估算 TPS */
function DurationCell({ record }: { record: UsageRecordItem }) {
  const first = record.firstTokenMs
  const dur = record.durationMs
  const tps = estimatedTps(record)
  if (first == null && dur == null) return <span className="text-ink-3">—</span>
  return (
    <div className="flex flex-col items-start gap-px">
      {first != null && (
        <span className={`${BADGE_CLS} ${durationTone(first, FIRST_TOKEN_FAST_MS, FIRST_TOKEN_SLOW_MS)}`} title="首字">
          首字 {formatDurationMs(first)}
        </span>
      )}
      {dur != null && (
        <span className={`${BADGE_CLS} ${durationTone(dur, DURATION_FAST_MS, DURATION_SLOW_MS)}`} title="耗时">
          耗时 {formatDurationMs(dur)}
        </span>
      )}
      {tps != null && (
        <span
          className={`${BADGE_CLS} ${tpsTone(tps)} font-mono tabular-nums`}
          title="估算输出速度：输出 tokens ÷ 生成耗时（流式扣除首字等待；不含上游隐藏推理）"
        >
          ≈ {tps >= 10 ? Math.round(tps) : tps.toFixed(1)} tok/s
        </span>
      )}
    </div>
  )
}

const USAGE_ERROR_LABELS: Record<UsageError, string> = {
  refused: '上游拒答',
  aborted: '上游中止',
  thinking_only_truncated: 'thinking 后无正文结束',
  empty: '空响应',
  empty_oversized_context: '空响应（上下文过大）',
  interrupted: '连接中断',
}

/** 请求以错误结束（上游已计费）时的红色小字说明（与 admin-ui 同款文案） */
function UsageErrorNote({ error }: { error: UsageError }) {
  return (
    <span
      className="block text-[10px] leading-tight text-danger"
      title="请求以错误结束，客户端没有拿到正常回复；上游已计费，因此记录在此"
    >
      {USAGE_ERROR_LABELS[error]}
    </span>
  )
}

/** 历史推理未能回传上游时的红色小字说明（与 admin-ui 同款文案） */
function ThinkingLossNote({ loss }: { loss: ThinkingLoss }) {
  const lines: string[] = []
  if (loss.signatureRejected) lines.push('思考丢失：签名被拒，已剥离')
  if (loss.unsignedTurns) lines.push(`思考丢失 ${loss.unsignedTurns} 轮（无签名）`)
  if (loss.renderedTurns) lines.push(`思考丢失 ${loss.renderedTurns} 轮（文本化思考）`)
  if (loss.invalidTurns) lines.push(`思考丢失 ${loss.invalidTurns} 轮（签名无效）`)
  return (
    <span
      className="block text-[10px] leading-tight text-danger"
      title="历史推理未能回传上游，模型看不到这些轮次之前的思考"
    >
      {lines.map((line) => (
        <span key={line} className="block">
          {line}
        </span>
      ))}
    </span>
  )
}

const LOG_COLS: { label: string; num?: boolean }[] = [
  { label: '时间' },
  { label: 'IP' },
  { label: '客户端' },
  { label: '模型' },
  { label: 'Token 用量' },
  { label: '用时' },
  { label: '费用 / Credits', num: true },
]

export function UsageLogTable({
  records,
  geoMap,
}: {
  records: UsageRecordItem[]
  geoMap: Map<string, GeoInfo | null>
}) {
  return (
    <table className="w-full border-separate border-spacing-0">
      <thead>
        <tr>
          {LOG_COLS.map((col) => (
            <th key={col.label} className={`${TH_BASE} ${col.num ? 'text-right' : ''}`}>
              {col.label}
            </th>
          ))}
        </tr>
      </thead>
      <tbody className="[&_tr:last-child>td]:border-b-0">
        {/* records are returned newest-first by the API */}
        {records.map((record, idx) => {
          const geo = record.clientIp ? geoMap.get(record.clientIp) : undefined
          const hitRate = cacheHitRate(record)
          return (
            <tr key={`${record.createdAt}-${record.model}-${idx}`} className="transition-colors hover:bg-surface-2">
              <td className={`${CELL} whitespace-nowrap text-[11.5px] text-ink-2`}>{formatDate(record.createdAt)}</td>
              <td className={`${CELL} whitespace-nowrap text-[11.5px] text-ink-2`}>
                {record.clientIp ? (
                  // IPv6 / 长地名不撑宽列：限宽截断，完整 IP 走 title（td 上的 max-width 浏览器不保证生效）
                  <span title={record.clientIp} className="flex max-w-[140px] flex-col">
                    <span className="truncate font-mono">{geo?.displayIp ?? record.clientIp}</span>
                    {/* city 缺失颗粒度的 IP（如 8.8.8.8）返回空串，拼接会留下悬空分隔符 */}
                    {geo?.country && (
                      <span className="truncate text-ink-3">
                        {geo.city ? `${geo.country}·${geo.city}` : geo.country}
                      </span>
                    )}
                  </span>
                ) : (
                  <span className="text-ink-3">—</span>
                )}
              </td>
              <td className={`${CELL} text-[11.5px] text-ink-2`}>
                {record.userAgent ? (
                  <span className="block max-w-[160px] truncate font-mono text-[10.5px]" title={record.userAgent}>
                    {uaShort(record.userAgent)}
                  </span>
                ) : (
                  !record.apiFormat && <span className="text-ink-3">—</span>
                )}
                {record.apiFormat && (
                  <span
                    className="mt-px block max-w-[160px] truncate text-[10px] text-ink-3"
                    title={`请求 API 格式: ${API_FORMATS[record.apiFormat].endpoint}`}
                  >
                    {API_FORMATS[record.apiFormat].label}
                  </span>
                )}
              </td>
              <td className={`${CELL} text-[11.5px]`}>
                <div className="flex flex-col items-start gap-px">
                  <span className={`font-mono ${getModelTone(record.model)}`}>{record.model}</span>
                  {(record.isStream != null || record.effort || record.thinkingBudgetTokens != null) && (
                    <div className="flex items-center gap-1">
                      {record.isStream != null && (
                        <span
                          className={`${BADGE_CLS} py-px leading-tight ${
                            record.isStream ? 'bg-brand-soft text-brand' : 'bg-surface-3 text-ink-2'
                          }`}
                        >
                          {record.isStream ? '流式' : '非流'}
                        </span>
                      )}
                      {record.effort && (
                        <span
                          className="rounded bg-orange-500/15 px-1 py-px font-mono text-[10px] leading-tight text-orange-500"
                          title={`effort: ${record.effort}`}
                        >
                          {record.effort}
                        </span>
                      )}
                      {record.thinkingBudgetTokens != null && (
                        <span
                          className="font-mono text-[10px] leading-tight tabular-nums text-ink-3"
                          title="思考预算 Tokens"
                        >
                          {formatTokens(record.thinkingBudgetTokens)}
                        </span>
                      )}
                    </div>
                  )}
                  {record.error && <UsageErrorNote error={record.error} />}
                  {record.thinkingLoss && <ThinkingLossNote loss={record.thinkingLoss} />}
                </div>
              </td>
              <td className={`${CELL} whitespace-nowrap text-[11px] text-ink-2`}>
                <div className="flex flex-col gap-px">
                  <span>
                    输入 Tokens：
                    <span className="font-mono tabular-nums">
                      {formatTokens(Math.max(0, record.inputTokens - (record.cacheReadInputTokens ?? 0)))}
                    </span>
                  </span>
                  <span className={(record.thinkingTokenCount ?? 0) > 0 ? 'flex items-center gap-1.5' : undefined}>
                    <span>
                      输出 Tokens：
                      <span className="font-mono tabular-nums">{formatTokens(record.outputTokens)}</span>
                    </span>
                    {record.thinkingTokenCount != null && record.thinkingTokenCount > 0 && (
                      <span
                        className="inline-flex items-center gap-0.5 rounded-full bg-warn-soft px-1.5 py-px font-mono text-[10px] font-semibold tabular-nums text-warn"
                        title="深度思考 Tokens"
                      >
                        <span className="sr-only">思考：</span>
                        <Brain className="size-3" aria-hidden="true" />
                        <span>{formatTokens(record.thinkingTokenCount)}</span>
                      </span>
                    )}
                  </span>
                  <span className="text-ok">
                    缓存读取（估算）：
                    <span className="font-mono tabular-nums">{formatTokens(record.cacheReadInputTokens ?? 0)}</span>
                    {hitRate != null && (
                      <span
                        className="ml-1.5 inline-flex items-center gap-0.5 rounded-full bg-brand-soft px-1.5 py-px align-middle font-mono text-[10px] font-semibold tabular-nums text-brand"
                        title={`估算缓存命中率 ${hitRate.toFixed(1)}%（缓存读取 / 输入总计；由 credits 反推或按静态前缀估算，非上游真值）`}
                      >
                        <DatabaseZap className="size-3" aria-hidden="true" />
                        {Math.round(hitRate)}%
                      </span>
                    )}
                  </span>
                  <span className="font-medium text-ink">
                    输入总计：
                    <span className="font-mono tabular-nums">{formatTokens(record.inputTokens)}</span>
                  </span>
                </div>
              </td>
              <td className={`${CELL} whitespace-nowrap text-[11px]`}>
                <DurationCell record={record} />
              </td>
              <td className={`${CELL} whitespace-nowrap text-right font-mono tabular-nums`}>
                <span className="block text-[12px] font-medium text-warn">{formatCost(record.estimatedCost)}</span>
                <span className="block text-[10.5px] text-brand">
                  {recordCredits(record).toFixed(4)} cr
                  {record.creditsUsed != null && <span className="ml-1 text-ok">✓</span>}
                </span>
                {record.creditsSaved != null && record.creditsSaved > 0 && (
                  <small className="block text-[10px] text-ok">省 {record.creditsSaved.toFixed(4)}</small>
                )}
              </td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}
