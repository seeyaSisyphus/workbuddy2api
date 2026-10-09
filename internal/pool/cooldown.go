// 冷却与熔断：Cooldown（固定时长账号级冷却）、CooldownSoftRate（账号级软冷却，对齐
// 上游重置时间或有界退避）、CooldownSoftForModel（模型级软冷却，对齐重置墙钟）、
// 软冷却封顶、熔断失败累计、签到解冻（ReenableIfCredits/reviveCoolingLocked）。
package pool

import (
	"strings"
	"time"
)

func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		p.dirty.Store(true)
	}
}

// SetCreditsDetailed 更新账号余额总量 + 快过架子集（签到时调用，供优先消耗快过期积分）。
// expiring 会被钳到 [0, credits]：上游分桶异常时不污染权重。
func (p *Pool) SetCreditsDetailed(uid string, credits, expiring int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if expiring < 0 {
			expiring = 0
		}
		if expiring > credits {
			expiring = credits
		}
		e.credits = credits
		e.creditsExpiring = expiring
		p.dirty.Store(true)
	}
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）。
//
// 重构后本入口是「固定时长的账号级冷却」，不再做两件旧事：
//   - 不再喂熔断器失败计数：熔断器只对「反复失败」（NoteError，5xx）退避。
//     软限流/余额耗尽各有权威恢复时刻（重置墙钟 / 04:00 签到），再并入"连续失败"
//     会让用户正常重试越堆越厚。熔断语义由 NoteError 唯一驱动（与 until 正交保持）。
//   - 不再做 softStreak 指数堆加：固定 d 即最终时长。CoolSoft 的精确对齐请用
//     CooldownSoftUntil（有界、对齐上游重置时间）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		// 非模型级冷却入口：清空 6004 模型级独立冷却表（modelCooldowns），
		// 避免上一次模型级限流的模型豁免泄漏到本次**账号级**限流上
		// （否则换模型请求会错误绕过本次冷却）。
		e.modelCooldowns = nil
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）：把该模型的冷却截止
// 精确对齐到上游重置墙钟（不做指数堆加、不做 softStreak 计数）。
//
//   - resetAt 非零（上游 body 的权威重置文案）→ modelCooldowns[model].Until =
//     min(resetAt+resetSkewBuffer, now+softRateResetMax)（见 cappedResetUntilLocked），
//     ResetAt 记录上游原始墙钟（台账 ResetAt）。不写 until（全账号级冷却不受模型级
//     限流污染），切模型即可用（模型豁免）。
//   - resetAt 零值（无时间文案）→ 有界退避：base 起按 softStreak 翻倍、封顶
//     softRateMax，且**在软冷却中**（until 未到期）时不推进/不延长（兜底探测不再把
//     冷却越堆越厚）。不记录模型（不豁免）。
//
// 与旧实现的差异：有上游重置时间时绝对不做指数堆加（旧实现 softStreak 计数含
// 死逻辑，纯属库房麻痹）；无重置时间时，「冷却中兜底探测再 429」不再 softStreak++
// 翻倍——这正是用户「全池被推到 2h 封顶」的元凶。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			// 上游权威重置墙钟：精确对齐（+缓冲、24h 兜底封顶），不做指数放大。
			if e.modelCooldowns == nil {
				e.modelCooldowns = map[string]modelCooldown{}
			}
			e.modelCooldowns[model] = modelCooldown{
				Until:   p.cappedResetUntilLocked(now, resetAt),
				ResetAt: resetAt,
				Reason:  reason,
			}
		} else {
			// 无解析时间（普通软冷却）：有界退避（base 起按 softStreak 翻倍、封顶
			// softRateMax）。注意：**在软冷却中**（until 未到期）时不推进/不延长。
			if e.coolKind != CoolSoft || !now.Before(e.until) {
				d := p.softDurationLocked(base, e.softStreak+1)
				e.softStreak++
				e.until = now.Add(d)
			}
			e.coolKind = CoolSoft
			e.reason = reason
			e.modelCooldowns = nil
		}
		p.dirty.Store(true)
	}
}

// BlockModelBackoff 11102「该后端无此模型」的 (账号, 模型) 负缓存入口（handler.applyErrorPolicy
// 调用）。复用 modelCooldowns 机制（不新建平行状态）：写 modelCooldowns[model]，Until 为指数退避
// TTL，选号侧 healthyForModel 自动对该账号避开该模型。
//
// 语义与 6004 正交：6004 是「模型被限流、对齐重置墙钟」，本入口是「官方确定该后端无此模型、
// 重试无意义，只能换模型/换账号」。退避按 reference model_blocks.py 口径（吸收语义、不抄实现）：
//   - 首次命中 TTL = modelBlockBaseTTL（6h）；
//   - 半开到期后允许放行重试；再次命中 Hits++ → TTL = base × 2^min(hits-1, modelBlockShift)；
//   - 封顶 modelBlockMaxTTL（24h，最多一天再试一次）；
//   - 该模型请求成功即由 BlockModelClear 清除（注意：6004 的 NoteSuccess 不清 modelCooldowns，
//     故清理必须由 handler 在成功且 model 命中 11102 条目时显式调用，见 BlockModelClear）。
//
// resetAt 无需传（11102 无重置文案），ResetAt 保持零值，与 6004 台账（rateLimitedModelsLocked）
// 共用 Until 判定——11102 条目会以 11102 reason 出现在 /status 台账，运维可见。
func (p *Pool) BlockModelBackoff(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	hits := 0
	if e.modelCooldowns != nil {
		hits = e.modelCooldowns[model].Hits
	}
	hits++
	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		ttl = d
	} else {
		ttl = modelBlockMaxTTL
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  now.Add(ttl),
		Reason: reason,
		Hits:   hits,
	}
	p.dirty.Store(true)
}

// BlockModelClear 清除 (账号, 模型) 的 11102 负缓存条目（该模型实测又通了）。半开探测或正常
// 请求对该模型成功后调用（handler 成功路径）。只清 11102 条目、不碰 6004 独立冷却表——
// 6004 有自身上游重置墙钟语义，成功不该抹掉（见 NoteSuccess 注释）。reason 前缀判定区分两者：
// 11102 条目的 reason 恒以 "11102" 开头（见上游 BlockModelReason）。
func (p *Pool) BlockModelClear(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	mc, exists := e.modelCooldowns[model]
	if !exists || !strings.HasPrefix(mc.Reason, "11102") {
		return
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.dirty.Store(true)
}

// CooldownSoftRate 429/限流文案的**账号级**软冷却入口（handler.applyErrorPolicy 调用）。
//
// 语义：
//   - resetAt 非零（上游带权威重置时间，无论 6004 还是 11140 rate-limiting）→
//     账号级直到该墙钟（截断到 softRateMax，绝不指数堆加）；**不**在
//     modelCooldowns 记模型（账号级语义，不产生切模型豁免，保持与旧
//     CooldownSoftForModel 无解析分支一致——普通账号级限流不该因切模型绕过）。
//   - resetAt 零值且**不在冷却中**（首次/恢复后的新限流）→ 有界退避：按 softStreak
//     指数退避并封顶 softRateMax（默认 2h，并经 softDurationLocked 统一封顶）。
//     softStreak 只在真正进入一次新冷却时计数，由 NoteSuccess/reviveCoolingLocked
//     清零（既有恢复语义）。
//   - resetAt 零值且**已在软冷却中**（兜底探测再次撞 429）→ 不推进 streak、不延长
//     until：用户重试/并发兜底探测不得把冷却越堆越厚——这正是旧实现「越重试越冷、
//     全池被推到 2h 封顶」的元凶（每次探测都 softStreak++ 指数翻倍）。
func (p *Pool) CooldownSoftRate(uid string, base time.Duration, resetAt time.Time, reason string) {
	// alignedReset=false：resetAt 由 Retry-After 头推导（可信度低于 body 权威文案），
	// 仍按 softRateMax 封顶——网页头可能给出离谱值，不该被它锁 24h。
	p.cooldownSoftRate(uid, base, resetAt, reason, false)
}

// CooldownSoftRateReset 同 CooldownSoftRate，但 resetAt 必须是**上游 body 解析出的权威
// 重置墙钟**（upstream.ParseRateReset，账号级 429/限流文案），冷却精确对齐该时刻
// （+90s 缓冲、24h 兜底封顶，见 cappedResetUntilLocked），不再被 softRateMax 截断。
//
// 为什么必须单列一个入口：softRateMax 默认 2h，而上游重置点常在 10~12h 后——按 2h 截断
// 等于提前 8~10 小时把号放出去重撞限流（2026-09-30 实测每 2 小时一轮，累计约 30 次无谓
// 上游调用）。调用方传入 Retry-After 推导的时刻时**必须**用 CooldownSoftRate，
// 否则一个网页头就能把号锁 24h。
func (p *Pool) CooldownSoftRateReset(uid string, base time.Duration, resetAt time.Time, reason string) {
	p.cooldownSoftRate(uid, base, resetAt, reason, true)
}

// cooldownSoftRate 两条账号级软冷却入口的共用实现。alignedReset 为真时 resetAt 按
// 权威重置墙钟对齐（cappedResetUntilLocked），为假时按 softRateMax 截断
// （cappedSoftUntilLocked）。调用方必须不持锁。
func (p *Pool) cooldownSoftRate(uid string, base time.Duration, resetAt time.Time, reason string, alignedReset bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			if alignedReset {
				e.until = p.cappedResetUntilLocked(now, resetAt)
			} else {
				e.until = p.cappedSoftUntilLocked(now, resetAt)
			}
		} else if e.coolKind != CoolSoft || !now.Before(e.until) {
			// 新限流（不在有效软冷却中）：推进有界退避；兜底探测（仍在软冷却中）不翻倍。
			d := p.softDurationLocked(base, e.softStreak+1)
			e.softStreak++
			e.until = now.Add(d)
		}
		e.coolKind = CoolSoft
		e.reason = reason
		e.modelCooldowns = nil // 账号级软冷却：清空模型豁免（切模型不绕过）
		p.dirty.Store(true)
	}
}

// cappedSoftUntilLocked 把**推导出来的**重置时刻截断到 softRateMax（now+softRateMax 与
// resetAt 取较早者）。resetAt 已过期（时钟偏移/文案过期）时日长短钳到时间零点附近，立即恢复。
//
// 与 cappedResetUntilLocked 的分野（调用方别用错）：本函数用于 Retry-After 头推导的时刻
// 与纯指数退避——网页头可信度低，且 WAF 是 IP 级、随时可能恢复，不该被它锁 24h。
// 调用方必须已持有 p.mu。
func (p *Pool) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	cap := now.Add(p.softRateMaxOr())
	if resetAt.After(cap) {
		return cap
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// softRateResetMax 上游**明确给出重置墙钟**时的冷却封顶（区别于指数退避的 softRateMax）。
//
// 为什么必须与 softRateMax 分开：上游 6004 的重置点常在 10~12 小时后，而 softRateMax
// 默认 2h——用 2h 截断等于提前 8~10 小时把号放出去，必然再撞一次 6004。2026-09-30 实测：
// 6 个 global 号被 6004 后，每 2 小时被放出去重撞一轮，循环到上游重置为止，累计约 30 次
// 无谓的上游调用 + 反复 503/429 风暴。
//
// 仍保留封顶而不是无条件信任上游文案：解析异常/时钟漂移时不能把号锁到几天后。
// 24h 覆盖真实重置窗口（实测最长 ~12h）并留足余量。
const softRateResetMax = 24 * time.Hour

// resetSkewBuffer 在上游重置墙钟**之后**再等的缓冲。按秒精确对齐时，本机时钟快几秒就会
// 让第一次重试正好落在上游重置之前，白吃一个 6004（上游按请求逐个落判）。90s 相对
// 10h 级窗口可忽略，却足以吸收常见时钟偏移与上游的落地延迟。
const resetSkewBuffer = 90 * time.Second

// cappedResetUntilLocked 把**上游权威重置墙钟**对齐为冷却截止：
// min(resetAt + resetSkewBuffer, now + softRateResetMax)。resetAt 已过期/不可信时立即恢复。
//
// 用于 resetAt 来自上游 body 权威重置文案的路径（upstream.ParseRateReset：6004 与
// 账号级 429）。这些路径下"提前解冻"没有任何收益——上游已经承诺了恢复时刻，提前重试
// 必然失败，只会白烧配额并给风控送样本。调用方必须已持有 p.mu。
func (p *Pool) cappedResetUntilLocked(now, resetAt time.Time) time.Time {
	if !resetAt.After(now) {
		return now.Add(time.Millisecond) // 已过期/时钟偏移：立即恢复
	}
	until := resetAt.Add(resetSkewBuffer)
	if cap := now.Add(softRateResetMax); until.After(cap) {
		return cap
	}
	return until
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号非禁用时，清冷却（余额恢复）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
// reviveCoolingLocked 已迁至 transition.go（状态机迁移唯一权威实现）。
