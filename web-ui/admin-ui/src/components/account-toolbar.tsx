// Copyright (c) 2026 Harllan He. Licensed under MIT.
import { useTranslation } from 'react-i18next'
import { SearchBox, Segmented, Toggle, Toolbar, UpdatedAgo, type SegmentedOption } from '@/components/toolbar'
import type { AccountStateGroup } from '@/lib/account-state'

/** 分段筛选口径：按 accountStateGroup 归组（与指标条 abnormalCount 同源） */
export type AccountStatusFilter = 'all' | AccountStateGroup

const SEGMENTS: { key: AccountStatusFilter; labelKey: string; pipClass?: string }[] = [
  { key: 'all', labelKey: 'credentials.filterAll' },
  { key: 'healthy', labelKey: 'credentials.filterHealthy', pipClass: 'bg-ok' },
  { key: 'abnormal', labelKey: 'credentials.filterAbnormal', pipClass: 'bg-warn' },
  { key: 'disabled', labelKey: 'credentials.filterDisabled', pipClass: 'bg-ink-3' },
  // 额度耗尽桶：与「已禁用」并列展示，避免跨月可恢复的账号被人工永久禁用桶吞掉
  { key: 'quota_exceeded', labelKey: 'credentials.filterQuotaExceeded', pipClass: 'bg-danger' },
  { key: 'pending', labelKey: 'credentials.filterPending', pipClass: 'bg-brand' },
]

export interface AccountToolbarProps {
  searchQuery: string
  onSearchChange: (value: string) => void
  statusFilter: AccountStatusFilter
  onStatusFilterChange: (value: AccountStatusFilter) => void
  /** 各分段计数（基于搜索过滤后的集合，由调用方按 deriveAccountState 计算） */
  counts: Record<AccountStatusFilter, number>
  /** TanStack Query 的 dataUpdatedAt；0 = 尚未取到数据，不渲染「更新于」 */
  dataUpdatedAt: number
  /** 选号是否无视账号冷却与降级（仅后端本次运行有效） */
  ignoreHealth: boolean
  onIgnoreHealthChange: (enabled: boolean) => void
}

/** 工具栏（设计稿 .toolbar）：搜索 + 状态分段筛选 + 无视冷却开关 + 更新时间 */
export function AccountToolbar({
  searchQuery,
  onSearchChange,
  statusFilter,
  onStatusFilterChange,
  counts,
  dataUpdatedAt,
  ignoreHealth,
  onIgnoreHealthChange,
}: AccountToolbarProps) {
  const { t } = useTranslation()
  const options: SegmentedOption<AccountStatusFilter>[] = SEGMENTS.map((seg) => ({
    key: seg.key,
    label: t(seg.labelKey),
    pipClass: seg.pipClass,
    count: counts[seg.key],
  }))

  return (
    <Toolbar>
      <SearchBox
        value={searchQuery}
        onChange={onSearchChange}
        placeholder={t('credentials.searchPlaceholder')}
        clearLabel={t('credentials.searchClear')}
      />
      <Segmented
        value={statusFilter}
        onChange={onStatusFilterChange}
        groupLabel={t('credentials.filterGroupLabel')}
        options={options}
      />
      {/* 开启后选号会持续请求冷却中的账号：开关周围换警示底色 */}
      <div
        title={t('credentials.ignoreHealthHint')}
        className={`flex h-[31px] shrink-0 items-center rounded-[7px] border px-2.5 transition-colors ${
          ignoreHealth ? 'border-warn-line bg-warn-soft text-warn' : 'border-transparent'
        }`}
      >
        <Toggle
          checked={ignoreHealth}
          onChange={onIgnoreHealthChange}
          label={t('credentials.ignoreHealth')}
          tone="warn"
        />
      </div>
      <UpdatedAgo dataUpdatedAt={dataUpdatedAt} />
    </Toolbar>
  )
}
