// Copyright (c) 2026 Harllan He. Licensed under MIT.
// 使用文档页开发指南与排障指南内容（自 codex2cc docsGuides 适配为 kiro2cc 端点与机制）
import type { DocsLocale } from './docsContent'

export type DocsSection = 'quick-start' | 'development' | 'model-api' | 'troubleshooting'

export type GuideSpec = {
  id: string
  section: DocsSection
  title: string
  summary: string
  /** 状态码胶囊：仅排障分区使用 */
  statusCode?: number
  paragraphs?: string[]
  table?: { headers: string[]; rows: string[][] }
  examples?: { label: string; content: string }[]
}

export function buildGuides(baseUrl: string, locale: DocsLocale): GuideSpec[] {
  const c = (zh: string, en: string) => (locale === 'zh' ? zh : en)
  const base = baseUrl.replace(/\/+$/, '')
  return [
    {
      id: 'authentication',
      section: 'development',
      title: c('地址与认证', 'URLs & authentication'),
      summary: c(
        '先确认请求发给谁，以及使用哪一种密钥。',
        'Choose the destination and the correct credential first.',
      ),
      paragraphs: [
        c(
          '客户端填写的是本服务的访问地址。账号的代理地址是服务访问上游时使用的出站代理，不是客户端 API Base URL。',
          'Clients connect to this service. An account proxy is the outbound proxy used when reaching Kiro upstream; it is not the client API Base URL.',
        ),
        c(
          '/v1/messages 与 /cc/v1/messages 使用后台创建的子 API Key（sk- 前缀），支持 Authorization: Bearer 或 x-api-key 两种请求头。管理后台（adminPsw）与子 Key 是两套独立凭据，不要混用。',
          '/v1/messages and /cc/v1/messages authenticate with a sub API key (sk- prefix) created in the dashboard, via either Authorization: Bearer or x-api-key. The admin panel (adminPsw) and sub keys are separate credentials; do not mix them.',
        ),
      ],
      table: {
        headers: [
          c('调用场景 / 客户端', 'Scenario / client'),
          c('分发目标地址', 'Destination URL'),
          c('认证 Header / 机制', 'Auth header / mechanism'),
          c('说明与特性', 'Notes & features'),
        ],
        rows: [
          [
            'Claude Code',
            `${base}`,
            'Authorization: Bearer sk-...',
            c(
              '填入 ANTHROPIC_BASE_URL 环境变量，客户端自动追加 /v1/messages。',
              'Put this into the ANTHROPIC_BASE_URL env var; the client appends /v1/messages itself.',
            ),
          ],
          [
            c('Claude Code（长会话）', 'Claude Code (long session)'),
            `${base}/cc`,
            'Authorization: Bearer sk-...',
            c('300s 全局截止时间，适配长会话不卡死。', '300s global deadline for long sessions.'),
          ],
          [
            'OpenAI SDK',
            `${base}/v1`,
            'Authorization: Bearer sk-...',
            c(
              'SDK 在 Base URL 后追加 /chat/completions 等资源路径。',
              'The SDK appends resource paths such as /chat/completions.',
            ),
          ],
          [
            c('公共 API', 'Public API'),
            `${base}/v1/messages`,
            'x-api-key: sk-...',
            c('Messages 端点也接受 x-api-key。', 'The Messages endpoints also accept x-api-key.'),
          ],
        ],
      },
    },
    {
      id: 'model-mapping',
      section: 'development',
      title: c('模型映射规则', 'Model mapping'),
      summary: c(
        '请求模型名按关键词路由到 Kiro 模型。',
        'Requested model names are keyword-routed to Kiro models.',
      ),
      paragraphs: [
        c(
          '模型名含 sonnet / opus / haiku / gpt 等关键词即路由到对应 Kiro 模型；版本号（4.5 / 4.6 / 4.7 / 4.8 / 5）决定具体代际，未明确指定版本时 opus 兜底 4.6。模型名含 thinking 后缀会自动启用扩展思考。',
          'A model name containing sonnet / opus / haiku / gpt keywords routes to the corresponding Kiro model; the version number (4.5 / 4.6 / 4.7 / 4.8 / 5) picks the generation, with opus falling back to 4.6 when unspecified. A thinking suffix enables extended thinking automatically.',
        ),
        c(
          'max_tokens 按模型代际有约束：下限 1024 对全部 Claude 代际生效，上限按代际分 128K / 64K 档；GPT 系模型跳过 additionalModelRequestFields 构建逻辑。',
          'max_tokens is constrained per generation: the 1024 floor applies to every Claude generation with 128K / 64K ceilings by tier; GPT-family models skip the additionalModelRequestFields build.',
        ),
      ],
      table: {
        headers: [
          c('请求模型名', 'Requested name'),
          c('路由目标', 'Routed to'),
          c('思考逻辑与代际约束', 'Thinking & generation constraints'),
        ],
        rows: [
          [
            'claude-sonnet-4-5',
            c('Sonnet 4.5 代际', 'Sonnet 4.5 generation'),
            c('含 sonnet + 4.5', 'contains sonnet + 4.5'),
          ],
          [
            'claude-opus-4-6-thinking',
            c('Opus 4.6 + 扩展思考', 'Opus 4.6 + extended thinking'),
            c('thinking 后缀启用思考', 'thinking suffix enables thinking'),
          ],
          [
            'gpt-5',
            c('GPT 系映射', 'GPT-family mapping'),
            c('跳过代际字段构建', 'skips generation field build'),
          ],
        ],
      },
    },
    {
      id: 'streaming',
      section: 'development',
      title: c('流式响应', 'Streaming responses'),
      summary: c(
        'Anthropic 与 OpenAI 兼容端点均支持 SSE 流式输出。',
        'Both the Anthropic and OpenAI-compatible endpoints support SSE streaming.',
      ),
      paragraphs: [
        c(
          'stream=true 时返回 text/event-stream：Anthropic 端点（/v1/messages、/cc/v1/messages）输出 message_start / content_block_delta / message_stop 事件序列；OpenAI 兼容端点输出 chunk 流并以 data: [DONE] 结束（Chat）或 response.completed 结束（Responses）。',
          'With stream=true the response is text/event-stream: the Anthropic endpoints (/v1/messages, /cc/v1/messages) emit message_start / content_block_delta / message_stop events; the OpenAI-compatible endpoints emit chunk streams terminated by data: [DONE] (Chat) or response.completed (Responses).',
        ),
        c(
          '思维链内容以 thinking 块返回；开启 thinkingAsText 设置后，思考块会被改写为逐行流出的文本（cli 入口带灰色 ANSI 样式，其余入口为 Markdown 引用块 + 斜体），便于在 Claude Code 中直接看到思考过程。用量统计在流的末尾事件中校准。',
          'Chain-of-thought is returned as thinking blocks; with the thinkingAsText setting on, thinking blocks are rewritten into line-by-line text (dim italic ANSI styling for the cli entrypoint, a Markdown blockquote with italics for other entrypoints) so Claude Code shows the reasoning inline. Usage is calibrated in the terminal events of the stream.',
        ),
      ],
    },
    {
      id: 'prompt-cache',
      section: 'development',
      title: c('Prompt 缓存机制', 'Prompt caching'),
      summary: c(
        'history[0] 冻结与前缀缓存。',
        'history[0] freeze and prefix caching.',
      ),
      paragraphs: [
        c(
          '服务通过冻结 history[0]（首条消息）保证 Kiro 侧前缀缓存跨请求逐字节稳定：系统提示中的计费哈希规范化、会话连续性 ID 派生等机制都为此服务。客户端侧不要在会话中途改写首条消息，否则上游缓存全量失效、首字延迟显著升高。',
          'The service freezes history[0] (the first message) so the Kiro prefix cache stays byte-stable across requests: billing-hash normalization in the system prompt and conversation-derived continuation IDs all serve this. Do not rewrite the first message mid-session on the client side, or the upstream cache fully invalidates and TTFT rises sharply.',
        ),
        c(
          'Kiro 的缓存按「账号 + 内容前缀」命中，5 分钟内复用会续期。服务会把同一会话、同一前缀的请求尽量路由回同一账号。用量中的 cache_read 依次取上游 metering 真值、按 credits 反推、同账号前缀观测估算；cache_creation 恒为 0。客户端看到的数值与用量日志一致。',
          'Kiro caches by account + content prefix; reuse within 5 minutes renews the entry. The service routes requests of the same session or prefix back to the same account where possible. Reported cache_read comes from upstream metering when present, otherwise from credits inference, otherwise from same-account prefix tracking; cache_creation is always 0. Clients see the same numbers as the usage log.',
        ),
      ],
    },
    {
      id: 'error-429',
      section: 'troubleshooting',
      statusCode: 429,
      title: c('429 与限流桶', '429 and rate-limit buckets'),
      summary: c(
        '上游 4 个端点对应 4 个独立限流桶。',
        'The 4 upstream endpoints map to 4 independent rate-limit buckets.',
      ),
      paragraphs: [
        c(
          'Kiro 上游按 ide / runtime / codewhisperer / amazonq 四个端点独立限流，桶级 429 按「凭据 + 端点」二元组隔离，单次 429 触发该桶 30 秒封禁。客户端看到的 429 通常意味着该账号在某端点配额耗尽，服务会自动故障转移到其他账号。',
          'Kiro upstream rate-limits the ide / runtime / codewhisperer / amazonq endpoints independently; a bucket-level 429 is isolated per (credential, endpoint) pair and bans that bucket for 30s. A client-facing 429 usually means the account exhausted quota on one endpoint; the service fails over to other accounts automatically.',
        ),
        c(
          '连续 429 时：在账号管理页确认账号余额与启用状态；调整账号优先级让流量优先走健康账号；或降低客户端并发。',
          'On repeated 429s: check account balance and enabled state on the Accounts page; adjust priorities so traffic prefers healthy accounts; or lower client concurrency.',
        ),
      ],
    },
    {
      id: 'error-401',
      section: 'troubleshooting',
      statusCode: 401,
      title: c('401 与认证失败', '401 and auth failures'),
      summary: c(
        '子 Key 无效、过期或额度耗尽。',
        'Invalid, expired or exhausted sub keys.',
      ),
      paragraphs: [
        c(
          '401 表示子 API Key 认证失败：Key 已删除/禁用、有效期已过、或额度（USD / credits 限额）耗尽。到 API Keys 页核对 Key 状态、有效期与消费额；懒激活 Key 首次使用才开始计时。',
          '401 means sub API key authentication failed: the key was deleted/disabled, expired, or hit its spending limit. Check key status, expiry and spend on the API Keys page; lazily-activated keys only start counting on first use.',
        ),
        c(
          '另一个 401 来源是上游账号 token 刷新失败：此时服务会按凭据状态标记账号异常并故障转移。到账号管理页查看失败日志与凭据状态。',
          'Another 401 source is upstream token refresh failure: the service marks the account abnormal and fails over. Check failure logs and credential state on the Accounts page.',
        ),
      ],
    },
    {
      id: 'error-5xx',
      section: 'troubleshooting',
      statusCode: 500,
      title: c('5xx 与上游异常', '5xx and upstream failures'),
      summary: c(
        '上游超时、账号失效，或多账号全部失败。',
        'Upstream timeouts, dead accounts, or every account failing.',
      ),
      paragraphs: [
        c(
          '单个请求最多对每个凭据重试 3 次、跨账号合计最多 9 次；普通请求 180s 超时，compact 请求 1000s。全部失败时才会把上游错误透传给客户端。',
          'A single request retries each credential up to 3 times and across accounts up to 9 times in total; normal requests time out at 180s and compact requests at 1000s. Only when everything fails is the upstream error surfaced to the client.',
        ),
        c(
          '持续 5xx 时：到账号管理页查看凭据状态与失败日志，确认至少一个账号可用；检查账号代理是否可达；必要时降低并发（全局 50 / 单账号 20）。',
          'On sustained 5xx: check credential state and failure logs on the Accounts page, confirm at least one account is usable, verify the account proxy is reachable, and lower concurrency if needed (global 50 / per account 20).',
        ),
      ],
    },
    {
      id: 'connection-check',
      section: 'troubleshooting',
      title: c('五步闭环连通性排查流程', 'Five-step connectivity checklist'),
      summary: c(
        '从服务在线到生成可用的五步排查。',
        'Five steps from service-up to working generation.',
      ),
      table: {
        headers: [
          c('排查步骤', 'Step'),
          c('操作与检查点', 'Action & checkpoint'),
          c('正常预期指标', 'Expected result'),
        ],
        rows: [
          [
            c('服务在线', 'Service up'),
            c(
              '打开管理后台，确认侧栏版本号与状态点。',
              'Open the admin panel and check the sidebar version and status dot.',
            ),
            c('页面正常加载，版本号可见。', 'Page loads and the version is visible.'),
          ],
          [
            c('子 Key 可用', 'Key usable'),
            c(
              'API Keys 页核对 Key 状态、有效期与额度。',
              'Check key state, expiry and quota on the API Keys page.',
            ),
            c('状态为启用，无过期或限额告警。', 'Enabled, with no expiry or quota warnings.'),
          ],
          [
            c('链路连通', 'Pipeline reachable'),
            c(
              '用快速接入页的 cURL 示例发一次 /v1/messages。',
              'Send one /v1/messages request with the cURL example from Quick start.',
            ),
            c('返回 message_stop 或完整 JSON。', 'A message_stop event or complete JSON comes back.'),
          ],
          [
            c('认证通过', 'Auth accepted'),
            c(
              '核对 Authorization: Bearer 或 x-api-key 请求头；命中 429 见「429 与限流桶」条目。',
              'Verify the Authorization: Bearer or x-api-key header; on 429 see the rate-limit entry.',
            ),
            c('无 401，响应内含 usage。', 'No 401 and the response carries usage.'),
          ],
          [
            c('内容正确', 'Output correct'),
            c(
              '内容异常时到「查看日志」开启 DEBUG 日志复现；再到使用日志核对最终模型、用量与耗时。',
              'On odd output, enable DEBUG logs in Log Viewer and reproduce; then confirm final model, usage and latency on the Usage page.',
            ),
            c('模型映射符合预期，token 计数正常。', 'Model mapping matches and token counts look sane.'),
          ],
        ],
      },
      examples: [
        {
          label: c('排查用最小请求（stream=false）', 'Minimal request (stream=false)'),
          content: `curl --request POST \\
  --url ${base}/v1/messages \\
  --header 'Authorization: Bearer sk-...' \\
  --header 'content-type: application/json' \\
  --data '{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"ping"}]}'`,
        },
      ],
    },
  ]
}
