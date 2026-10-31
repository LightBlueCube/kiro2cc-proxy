//! 模型定价与费用估算：每百万 tokens 美元定价、credits/USD 换算率、单次请求费用。
/// 模型定价（每百万 tokens，美元）
/// 使用 200K context 标准定价
pub(crate) struct ModelPricing {
    input_per_mtok: f64,
    output_per_mtok: f64,
}

/// 判断模型名是否属于 haiku 5.5（5.5 代际）。
///
/// 与 `crate::anthropic::converter::model::is_haiku_55` 保持同一判定口径
/// （同一模型族在映射层与定价层必须一致，否则会出现「映射到 5.5 但按 4.5 计价」）。
/// 不跨层引用是为避免 `model` 层反向依赖 `anthropic` 层。
fn is_haiku_55(model_lower: &str) -> bool {
    model_lower.replace(['.', ' '], "-").contains("haiku-5-5")
}

/// Haiku 5.5 长上下文阈值（tokens）。
///
/// 官方口径：单次请求 prompt 超过 100K 时，**整个请求**按 5 倍高档计费
/// （非超出部分累进）。低档 $0.10/$0.50，高档 $0.50/$2.50。
const HAIKU_55_LONG_CONTEXT_THRESHOLD: i32 = 100_000;

/// 触发 `creditsUsed` 缓存读取反推的最小输入 token 阈值。
///
/// 输入过短时，冷/热两条计费曲线差值过小，输出分词微差会被放大；
/// 且 1.5K 以内首段单价更高（实测曲线仅在 4K 以上呈直线），故直接保留静态前缀估算。
pub(crate) const MIN_INFER_INPUT_TOKENS: i32 = 4096;

/// 反推命中比例允许超出 [0, 1] 的幅度；超出即视为模型无法解释本次计费，不反推。
///
/// 实测曲线残差 ≤ 5%，余量覆盖输出分词与隐藏推理估算误差。
const HIT_RATIO_TOLERANCE: f64 = 0.15;

/// Haiku 5.5 长上下文跳档的本地 token 判定区间（左闭右开）。
///
/// 上游按自身分词数超 100K 跳档，折成本地 cl100k 计数随内容变化
/// （2026-10-29 实测跳档点：Rust 源码约 64K、随机英文词 61K–65K、中文 70K–80K）。
/// 区间内两档都可能，由哪一档能解释本次计费决定（见 `infer_cache_read_from_credits`）。
const HAIKU_55_LONG_CONTEXT_LOCAL_RANGE: std::ops::Range<i32> = 60_000..80_000;

/// Kiro 实测 credits 计费曲线（2026-10-29 pinned 单账号重标定，Rust 源码语料，输入 2.7K–97K）。
///
/// 上游计费可由「全未命中」与「全命中」两条直线描述，命中比例在两者之间线性插值：
///
/// ```text
/// credits   = cold(I) − hit · (cold(I) − cached(I)) + output_per_mtok · O
/// cold(I)   = cold_base   + cold_per_mtok   · I
/// cached(I) = cached_base + cached_per_mtok · I
/// ```
///
/// 截距包含 Kiro 注入的隐藏系统提示（本地 token 计数不含）。opus / sonnet 实测：
/// 新增尾部不短于已缓存前缀时整单按 cold 计价，否则部分命中；haiku 为常规前缀缓存。
/// 插值直接给出「计费意义上的」命中比例，两种行为无需分别建模。
///
/// 同样本地 token 数的计费随内容而变：相对 Rust 源码，随机英文词约 +3%、
/// 英文散文约 +1.5%、中文约 −16%（纯中文冷请求会被估成约 20% 命中）。
struct CreditCurve {
    cold_base: f64,
    cold_per_mtok: f64,
    cached_base: f64,
    cached_per_mtok: f64,
    /// 散文类输出单价（cl100k 口径）；纯数字列表实测约低 30%
    output_per_mtok: f64,
    /// 隐藏推理输出 tokens ≈ signature_slope · 签名字符数 + signature_intercept · 签名块数
    signature_slope: f64,
    signature_intercept: f64,
    /// 回传历史推理按输入计费的量：每个隐藏推理 token 折合的本地输入 token
    /// （与普通输入同享缓存折扣；sonnet / haiku 按新一轮回传标定）。
    /// opus 实测回传不计费：新一轮回传模型看不到，工具循环内回传模型看得到，
    /// 但两种位置都不计费（2026-10-31，约 1 万隐藏推理 token 回传，credits 与不回传持平）
    replay_input_per_hidden_token: f64,
}

const OPUS_55_CURVE: CreditCurve = CreditCurve {
    cold_base: 0.02293,
    cold_per_mtok: 18.801,
    cached_base: 0.00696,
    cached_per_mtok: 5.081,
    output_per_mtok: 350.0,
    // 每签名字符的 credits 与 2026-10-26 标定一致，按新输出单价折算
    signature_slope: 0.2566,
    signature_intercept: -142.4,
    replay_input_per_hidden_token: 0.0,
};

const SONNET_55_CURVE: CreditCurve = CreditCurve {
    cold_base: 0.01495,
    cold_per_mtok: 12.220,
    cached_base: 0.00454,
    cached_per_mtok: 3.303,
    output_per_mtok: 208.0,
    signature_slope: 0.2672,
    signature_intercept: -110.1,
    replay_input_per_hidden_token: 1.48,
};

/// Haiku 5.5 常态档。
const HAIKU_55_CURVE_STANDARD: CreditCurve = CreditCurve {
    cold_base: 0.00014,
    cold_per_mtok: 1.768,
    cached_base: 0.00023,
    cached_per_mtok: 0.177,
    output_per_mtok: 8.4,
    signature_slope: 0.2989,
    signature_intercept: -170.7,
    replay_input_per_hidden_token: 0.945,
};

/// Haiku 5.5 长上下文档（整单 5 倍跳价）。输出单价按 5 倍推算，未实测。
const HAIKU_55_CURVE_LONG_CONTEXT: CreditCurve = CreditCurve {
    cold_base: 0.00179,
    cold_per_mtok: 8.835,
    cached_base: 0.00128,
    cached_per_mtok: 0.883,
    output_per_mtok: 42.0,
    signature_slope: 0.2989,
    signature_intercept: -170.7,
    replay_input_per_hidden_token: 0.945,
};

/// 常态档在前、长上下文档在后，`credit_curves` 按跳档区间切片
static HAIKU_55_CURVES: [CreditCurve; 2] = [HAIKU_55_CURVE_STANDARD, HAIKU_55_CURVE_LONG_CONTEXT];

/// Opus 4.5+ 定价：$5 / $25。
const OPUS_PRICING: ModelPricing = ModelPricing {
    input_per_mtok: 5.0,
    output_per_mtok: 25.0,
};

/// 根据模型名获取定价
pub(crate) fn get_model_pricing(model: &str) -> ModelPricing {
    pricing_of_lower(&model.to_lowercase())
}

fn pricing_of_lower(model_lower: &str) -> ModelPricing {
    if model_lower.contains("opus") {
        OPUS_PRICING
    } else if is_haiku_55(model_lower) {
        // Haiku 5.5: $0.10 / $0.50（≤100K 低档，官方称覆盖约 90% 请求量）。
        // >100K 的高档由 calculate_cost 按整档跳价处理（本函数不持有 token 数）。
        ModelPricing {
            input_per_mtok: 0.10,
            output_per_mtok: 0.50,
        }
    } else if model_lower.contains("haiku") {
        // Haiku 4.5: $1 / $5
        ModelPricing {
            input_per_mtok: 1.0,
            output_per_mtok: 5.0,
        }
    } else {
        // Sonnet 4 / sonnet-5 / haiku: $3 / $15
        // claude-sonnet-5 Rate = 1.3 Credit，与 sonnet-4.x 同档，定价一致
        ModelPricing {
            input_per_mtok: 3.0,
            output_per_mtok: 15.0,
        }
    }
}

/// 平台级 credits/USD 换算率，按模型档位差异化（代理实测 2026-06-25）。
/// 供 usage 报表 credits_saved（estimated_cost × k_ref - credits_used）使用。
/// 2026-06-30 重校：按 opus 版本分档，基于实测 d=0.50 缓存折扣反推。
/// 此处保留历史标定背景；缓存读取反推已改用实测计费曲线（`CreditCurve`），不依赖本值。
/// 2026-07-25 追加 opus-5 与 4.7/4.8 同档。
pub(crate) fn get_k_ref(model: &str) -> f64 {
    k_ref_of_lower(&model.to_lowercase())
}

fn k_ref_of_lower(m: &str) -> f64 {
    if m.contains("opus-4-7")
        || m.contains("opus-4.7")
        || m.contains("opus-4-8")
        || m.contains("opus-4.8")
        || m.contains("opus-5")
        || m.contains("opus.5")
    {
        // opus 4.7/4.8/5 共用同档（实测 4.8 ≈ 2.36，5 沿用 4.8 档位）
        2.36
    } else if m.contains("opus-4-5")
        || m.contains("opus-4.5")
        || m.contains("opus-4-6")
        || m.contains("opus-4.6")
    {
        // 旧 opus 4.5/4.6（实测 4.6 ≈ 1.90）
        1.90
    } else if m.contains("opus") || m.contains("fable") {
        // 未知 opus / fable 兜底沿用最新档
        2.36
    } else if m.contains("sonnet-5") || m.contains("sonnet.5") {
        // claude-sonnet-5: Rate = 1.3 Credit，与 sonnet-4.5/4.6 同档（实测确认）
        1.43
    } else {
        // sonnet 系列 / haiku（含 haiku 5.5）默认
        1.43
    }
}

/// 计算单次请求的估算费用
///
/// Haiku 5.5 按官方「整档跳价」口径：输入超 100K 时整个请求走高档
/// （见 HAIKU_55_LONG_CONTEXT_THRESHOLD）。
///
/// 已知偏差：`input_tokens` 为**不含 cache token** 的净输入
/// （cache_read / cache_creation 单独字段存储，未参与定价计算），
/// 故阈值按净输入判定，与官方「prompt 总长度」口径存在系统性低估，
/// 带长缓存前缀的会话可能被判入低档。
pub(crate) fn calculate_cost(model: &str, input_tokens: i32, output_tokens: i32) -> f64 {
    let model_lower = model.to_lowercase();
    let pricing = if is_haiku_55(&model_lower) && input_tokens > HAIKU_55_LONG_CONTEXT_THRESHOLD {
        ModelPricing {
            input_per_mtok: 0.50,
            output_per_mtok: 2.50,
        }
    } else {
        get_model_pricing(model)
    };
    let input_cost = (input_tokens as f64 / 1_000_000.0) * pricing.input_per_mtok;
    let output_cost = (output_tokens as f64 / 1_000_000.0) * pricing.output_per_mtok;
    input_cost + output_cost
}

/// 本次响应的计费输出观测值，供 credits 反推估算上游真实输出。
///
/// 原生推理只回传摘要，真实推理输出隐藏在签名里：签名长度与隐藏输出强相关
/// （实测 R² 0.94–0.98），摘要 tokens 则只有 0.7–12.7 倍的松散关系。
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct BilledOutput {
    /// 本地 BPE 计费口径输出 tokens（可见输出 + 推理摘要 + 旧标签 thinking）
    pub tokens: i32,
    /// `tokens` 中的原生推理摘要部分
    pub native_reasoning_tokens: i32,
    /// 原生推理签名字符数之和
    pub signature_chars: usize,
    /// 带签名的原生推理块数
    pub signature_blocks: usize,
}

/// 本次请求历史中实际回传上游的推理签名（本地输入 token 计数不含）。
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct ReplayedReasoning {
    /// 回传签名字符数之和
    pub signature_chars: usize,
    /// 回传的推理块数
    pub blocks: usize,
}

/// 已实测标定模型在本次输入长度下的候选计费曲线；未标定模型为空（跳过反推，保留静态前缀估算）。
///
/// haiku 5.5 在跳档区间内返回两档，其余情况恰好一条。
fn credit_curves(model: &str, input_tokens: i32) -> &'static [CreditCurve] {
    let model_lower = model.to_lowercase();
    let normalized = model_lower.replace(['.', ' '], "-");
    if normalized.contains("opus-5-5") {
        std::slice::from_ref(&OPUS_55_CURVE)
    } else if normalized.contains("sonnet-5-5") {
        std::slice::from_ref(&SONNET_55_CURVE)
    } else if is_haiku_55(&model_lower) {
        if input_tokens < HAIKU_55_LONG_CONTEXT_LOCAL_RANGE.start {
            &HAIKU_55_CURVES[..1]
        } else if input_tokens >= HAIKU_55_LONG_CONTEXT_LOCAL_RANGE.end {
            &HAIKU_55_CURVES[1..]
        } else {
            &HAIKU_55_CURVES
        }
    } else {
        &[]
    }
}

impl CreditCurve {
    /// 按签名长度估算的隐藏推理 tokens
    fn hidden_reasoning_tokens(&self, signature_chars: usize, blocks: usize) -> f64 {
        self.signature_slope * signature_chars as f64 + self.signature_intercept * blocks as f64
    }

    /// 上游计费的输出 tokens：推理摘要替换为按签名估算的隐藏推理输出（不低于摘要）。
    ///
    /// 有推理摘要却无签名时无法估算隐藏输出，返回 `None`。
    fn upstream_output_tokens(&self, output: &BilledOutput) -> Option<f64> {
        let summary = output.native_reasoning_tokens as f64;
        if output.native_reasoning_tokens > 0 && output.signature_blocks == 0 {
            return None;
        }
        let from_signature =
            self.hidden_reasoning_tokens(output.signature_chars, output.signature_blocks);
        Some(output.tokens as f64 - summary + summary.max(from_signature))
    }

    /// 回传推理折合的本地输入 tokens
    fn replayed_input_tokens(&self, replayed: &ReplayedReasoning) -> f64 {
        let hidden = self.hidden_reasoning_tokens(replayed.signature_chars, replayed.blocks);
        (self.replay_input_per_hidden_token * hidden).max(0.0)
    }

    /// 本次计费对应的命中比例（未截断，可能超出 `[0, 1]`）；无法估算输出时为 `None`。
    ///
    /// 回传推理并入输入 `I`：它和普通输入一样按冷价 / 缓存价计费。
    fn hit_ratio(
        &self,
        input_tokens: i32,
        replayed: &ReplayedReasoning,
        output: &BilledOutput,
        credits_used: f64,
    ) -> Option<f64> {
        let upstream_output = self.upstream_output_tokens(output)?;
        let input_mtok = (input_tokens as f64 + self.replayed_input_tokens(replayed)) / 1_000_000.0;
        let cold = self.cold_base + self.cold_per_mtok * input_mtok;
        let cached = self.cached_base + self.cached_per_mtok * input_mtok;
        let output_cost = self.output_per_mtok * upstream_output / 1_000_000.0;
        Some((cold + output_cost - credits_used) / (cold - cached))
    }
}

/// 命中比例落在 `[−0.15, 1.15]` 内才算该曲线能解释本次计费
fn is_explainable(hit_ratio: f64) -> bool {
    (-HIT_RATIO_TOLERANCE..=1.0 + HIT_RATIO_TOLERANCE).contains(&hit_ratio)
}

/// 比全未命中价还贵（超出容差）：计费里没有缓存折扣，确定未命中
fn is_above_cold(hit_ratio: f64) -> bool {
    hit_ratio < -HIT_RATIO_TOLERANCE
}

/// 由上游 `meteringEvent.usage`（`credits_used`）反推计费意义上的缓存读取 token 数。
///
/// `hit = (cold(I) + r_out · O − credits) / (cold(I) − cached(I))`，返回 `round(clamp(hit, 0, 1) · I)`，
/// 其中 `I` 含回传推理折合的输入（见 [`CreditCurve::hit_ratio`]），返回值按本地输入计。
/// 仅对已标定模型、`input_tokens >= 4096`、`credits_used > 0` 生效。
///
/// 恰有一条候选曲线能解释时按它反推；所有候选曲线都判定比冷价还贵时返回 `Some(0)`
/// （2026-10-30 线上：opus-5.5 输入 110903、输出 436 计费 2.59 credits，比冷价曲线加输出
/// 高约 15%；此前退回静态前缀估算，记成 99% 命中）。
/// 其余情况返回 `None`：比全命中价还便宜（曲线无法解释），无法估算输出，
/// 或 haiku 跳档区间内两档都能解释。
pub(crate) fn infer_cache_read_from_credits(
    model: &str,
    input_tokens: i32,
    replayed: &ReplayedReasoning,
    output: &BilledOutput,
    credits_used: f64,
) -> Option<i32> {
    if input_tokens < MIN_INFER_INPUT_TOKENS || !credits_used.is_finite() || credits_used <= 0.0 {
        return None;
    }
    let hit_ratios: Vec<f64> = credit_curves(model, input_tokens)
        .iter()
        .filter_map(|curve| curve.hit_ratio(input_tokens, replayed, output, credits_used))
        .collect();

    let mut explained = hit_ratios.iter().copied().filter(|&r| is_explainable(r));
    match (explained.next(), explained.next()) {
        (Some(hit_ratio), None) => {
            Some((hit_ratio.clamp(0.0, 1.0) * input_tokens as f64).round() as i32)
        }
        // 长上下文档全命中 ≈ 常态档冷价一半，与常态档约 55% 命中无法区分
        (Some(_), Some(_)) => None,
        (None, _) => {
            let surely_missed =
                !hit_ratios.is_empty() && hit_ratios.iter().all(|&r| is_above_cold(r));
            surely_missed.then_some(0)
        }
    }
}

/// 结合静态前缀上界与 `creditsUsed` 反推值校准最终 `cache_read_input_tokens`。
///
/// - `prefix_estimated_tokens <= 0`（首轮或 `history[0]` 重置）时锁定为 `0`；
/// - 满足反推条件时取 `min(prefix_estimated_tokens, inferred_read, final_input_tokens)`
///   （比冷价还贵时反推值为 0）；
/// - 其余情况（`< 4096`、无 `credits_used`、未标定模型、比全命中价还便宜）保留
///   `min(prefix_estimated_tokens, final_input_tokens)`。
pub(crate) fn refine_prefix_cache_read(
    model: &str,
    prefix_estimated_tokens: i32,
    final_input_tokens: i32,
    replayed: &ReplayedReasoning,
    output: &BilledOutput,
    credits_used: Option<f64>,
) -> i32 {
    if prefix_estimated_tokens <= 0 || final_input_tokens <= 0 {
        return 0;
    }
    let capped_prefix = prefix_estimated_tokens.min(final_input_tokens);
    match credits_used.and_then(|cu| {
        infer_cache_read_from_credits(model, final_input_tokens, replayed, output, cu)
    }) {
        Some(inferred) => capped_prefix.min(inferred),
        None => capped_prefix,
    }
}
