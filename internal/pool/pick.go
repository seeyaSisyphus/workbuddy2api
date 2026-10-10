// 选号：Pick 簇（healthy 三因子加权 Top5 短名单 + 加权随机 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知）。
// DeptestOnly: 全库仅 pool 包测试引用；生产选号全走 PickExcludingForRealm /
// PickByUIDForModel。保留是因为测试需要无轮换/无 realm 的最小选号原语；
// 迁 export_test.go 不可行——export_test 对包外不可见，而本方法的语义文档
// （挑选策略全文）对生产簇（pick 私有实现）仍有维护参考价值。
// 挑选策略：healthy 账号中按三因子权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
// model 非空时启用 6004 模型级冷却豁免（healthyForModel）；空则等价账号级 healthy。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick(model string) *auth.Auth {
	return p.pick(nil, model, "")
}

// pick 在 healthy 候选集中按三因子权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// 候选集是 top5 近似：先按三因子权重（weightOf）降序取前 5（credits 只是权重的一个因子，
// 闲置补偿与成功率同样决定谁进短名单），再在 top5 内做防撞号过滤。
// 并发防雪崩：跳过 lastUsed 距今 < minPickGap 的账号（除非 top5 全部刚被用过，
// 此时退回最近最少使用 LRU 账号），迫使高并发请求发散，而不是全部撞同一高分账号。
// minPickGap=0（测试用）时过滤恒通过，退化为纯加权随机。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效；PickExcluding 传 ""）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域；PickExcluding 传 ""）。
// 注意：模型豁免只进 normal 选号（候选 healthy 判定）；全冷却兜底**反而显式排除**在
// reqModel 上被上游拒过（6004 配额 / 11102 负缓存）且未到恢复时刻的账号——上游已承诺
// 恢复时刻，兜底提前探测必然失败，只会白烧配额。切模型的可用性已在 normal 阶段体现。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 惰性清理过期的 6004 模型级冷却与成本台账（两者的 map 都不无限膨胀；
	// status 只读遍历天然跳过过期项，但内存条目必须在此真正删除，上游 dcc4918）。
	for _, e := range p.byUID {
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
	}
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !healthyOf(e) {
			continue
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底；
		// reqModel 上被上游拒过（6004/11102）且未到恢复时刻的账号同样排除。
		return p.pickEarliestExpiryLocked(tried, now, realm, reqModel)
	}
	// 第 0 层（积分底线，硬过滤，先于一切分层）：credits < lowCreditStop 的候选跳过——
	// 余额见底的号继续打只会撞 402 硬冷却到次日 04:00。豁免两种：
	//  1. creditsExpiring >= stop（快过期的分不用白不用，作废前烧掉比保住本金更值）；
	//  2. 该号对请求模型持有有效「实测免费」观测（成本账本 tier 0）——限免期请求不耗
	//     积分，底线前提不成立；观测 6h TTL 过期或限免结束（首次收费观测灌入 EMA 把
	//     CostPer1k 拉离 0）后豁免自动失效，无需人工摘除。
	// 豁免按（账号, 模型）各自判定，不用池级证据：限免若实为部分账号专享，池级豁免会
	// 把没份的低余额号放出去白烧到 402 硬冷却。
	// 若过滤后无候选 → 跳过本层降级使用全部候选（weightOf 会自然偏好高余额号），记 WARN 暴露。
	// 兜底路径（pickEarliestExpiryLocked）不套本层：它已排除 CoolHard（余额耗尽）号，
	// 其余冷却中的低余额号只是"无健康号时的降级探测"，一次探测的成本远小于无号可服务的损失。
	if p.lowCreditStop > 0 {
		// freeObserved 该号对请求模型的实测免费观测仍有效（成本账本 tier 0）。
		// modelCostOf 对空 reqModel 恒 false，非模型感知路径不触发本豁免。
		freeObserved := func(e *entry) bool {
			mc, ok := e.modelCostOf(reqModel, now)
			return ok && mc.CostPer1k <= 0
		}
		kept := cands[:0]
		for _, e := range cands {
			if e.credits < p.lowCreditStop && e.creditsExpiring < p.lowCreditStop && !freeObserved(e) {
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) > 0 {
			cands = kept
		} else {
			log.Printf("WARN: [pool] low_credit_stop=%d 过滤后无候选（%d 个全部低于底线），跳过底线层降级使用", p.lowCreditStop, len(cands))
		}
	}
	// top5 短名单按三因子权重降序截断（而非 credits 单纯降序）：否则闲置补偿 + 成功率
	// 根本进不了短名单决策，低 credits 但高成功率/久置的账号会永远排不进 top5。
	// maxCredits 统一用**全集口径**（tier 过滤前的全部 healthy 候选）：截断排序与
	// 抽签权重共享同一基准，两个阶段权重可比（旧实现 pickWeighted 在 eligible 子集
	// 上重取 max，全集最大 credits 号被 minPickGap 挤出后子集 max 偏小，剩余号的
	// credits 比例整体膨胀 k=全集max/子集max，credits 项相对 idle/success 被放大）。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// maxExpiring 快过期积分的全集最大值（同上全集口径），供 expiring 权重项归一化。
	// 用**绝对量**而非"占比"：占比会让"100 分里 90 分快过期"压过"10000 分里 5000 分
	// 快过期"，而后者作废的绝对损失大 50 倍；选号目标是让快作废的积分尽量被用掉。
	var maxExpiring int64
	for _, e := range cands {
		if e.creditsExpiring > maxExpiring {
			maxExpiring = e.creditsExpiring
		}
	}
	// 快过期分层（最高优先级，硬过滤）：有快过期积分的账号整体优先于没有的账号——
	// 快过期积分不用就作废，必须先消耗；等它们清零后（NoteModelCost 随消耗递减 /
	// 定期刷新回写）本层自然消失，选号回落到按积分总量的常规口径。
	// 这是**外层**分层：外层决定"是否在消耗快过期积分"，内层 costTier 决定
	// "同层内优先免费"，两者正交（见下）。
	expiringTier := func(e *entry) int {
		if e.creditsExpiring > 0 {
			return 0
		}
		return 1
	}
	bestExp := 1
	for _, e := range cands {
		if t := expiringTier(e); t < bestExp {
			bestExp = t
		}
	}
	if bestExp == 0 {
		kept := cands[:0]
		for _, e := range cands {
			if expiringTier(e) == 0 {
				kept = append(kept, e)
			}
		}
		cands = kept
	}
	// 权重与成本分层**各算一次、全程复用**（weighted 结构体定义见包级注释）：
	// weightOf 每候选一次（预计算存入 ws.w），costTier/modelCostOf 同样每候选一次
	// （存入 ws.tier/ws.cost1k）——sort 比较器与 pickWeighted 都只读缓存字段，
	// 不再现算。比较器内现算会翻成 O(n log n) 次冗余浮点/map 查找（46 账号约
	// 500 次比较），旧实现在此翻过车。
	// 成本分层：reqModel 非空时，按该模型的实测扣费把候选分层，只保留最优层。
	//   0 = 已实测免费（限免期/夜间免费的号，最强偏好）
	//   1 = 无观测（含观测过期）
	//   2 = 已实测收费
	// 为什么"无观测"排在"已实测收费"之前：新号的限免状态只能靠实测发现，
	// 若已知收费的号恒压过未知号，那台免费的号永远轮不到，也就永远学不到。
	// 为什么用硬过滤而非仅排序：pickWeighted 会在候选内加权随机，只排序的话
	// 收费号仍有机会抽中，达不到"优先免费"的语义。
	costTier := func(e *entry) (int, float64) {
		mc, ok := e.modelCostOf(reqModel, now)
		if !ok {
			return 1, 0
		}
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	bestTier := 2
	for _, e := range cands {
		if ti, _ := costTier(e); ti < bestTier {
			bestTier = ti
		}
	}
	ws := make([]weighted, 0, len(cands))
	for _, e := range cands {
		ti, ci := costTier(e)
		if ti == bestTier {
			ws = append(ws, weighted{e: e, w: p.weightOf(e, maxCredits, maxExpiring, now), tier: ti, cost1k: ci})
		}
	}
	// 等权重洗牌：仅当存在权重并列（epsilon 比较，防浮点微差让洗牌静默失效）且
	// 候选数超过 top5 时，才对 ws 做 Fisher-Yates 洗牌（且**不消耗 p.randInt64N
	// 注入源**，避免改变 pickWeighted 的确定性语义，见 TestPickDeterministicViaSet
	// RandomSource）。权重全等或存在并列时，按字典序截断会让 uid 靠后的账号永远
	// 进不了 top5（惊群测试 c00 集中 79/100 的根因：c05..c09 被字典序截断、LRU
	// 兜底又只在 top5 内转）。洗牌用独立的 time-seeded 源，只在截断边界制造等
	// 权重随机次序，不影响加权抽签本身的确定性。
	if len(ws) > 5 {
		eq := false
		for i := 1; i < len(ws); i++ {
			if math.Abs(ws[i].w-ws[0].w) < weightEpsilon {
				eq = true
				break
			}
		}
		if eq {
			shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(ws))))
			shuf.Shuffle(len(ws), func(i, j int) { ws[i], ws[j] = ws[j], ws[i] })
		}
	}
	sort.SliceStable(ws, func(i, j int) bool {
		// costTier 硬过滤后 ws 全员同层，但仍按 cost1k 升序排（tier 2 层内单价低者
		// 在前；tier 0/1 层 cost1k 恒 0，本比较退化为权重比较）——读缓存字段不现算。
		if ws[i].cost1k != ws[j].cost1k {
			return ws[i].cost1k < ws[j].cost1k // 收费层：单价低的在前
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID // 稳定兜底（洗牌后此项几乎不触发）
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	// candsAll 保留截断前的全候选（权重降序），供 LRU 兜底在全量范围选最旧者，
	// 避免 top5 字典序截断把等权重靠后账号饿死（惊群根因之一）。
	candsAll := cands
	if len(cands) > 5 {
		cands = cands[:5]
	}
	// 防并发撞号：在持锁内基于「上次选中时刻」过滤，但同一批并发 goroutine 会串行进入
	// 本函数（写锁），每个进入者都把 lastUsed 置为 now —— 于是同一瞬间的第 2..N 个
	// 进入者看到前一个账号 lastUsed==now（距今 0 < minPickGap），被自然挤向其他账号。
	// 关键：lastUsed 在锁内赋值，使时间窗口判定在并发下可重入（此前 Acquire 在锁外，
	// 多个 goroutine 在窗口内同时通过校验造成惊群，TestPickAntiThunderingHerd 实证）。
	// 注意：先截断后过滤（截断边界内被挤出的号不回填）——与旧实现语义严格一致。
	eligible := make([]weighted, 0, len(cands))
	for _, we := range ws[:min(len(ws), 5)] {
		if now.Sub(we.e.lastUsed) >= minPickGap {
			eligible = append(eligible, we)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// top5 全部刚被用过：LRU 兜底，在**全候选 candsAll**（非仅 top5）里选最旧者。
		// 用 usedSeq 单调序号而非 lastUsed 墙钟比较：Windows 等平台 time.Now() 精度
		// ~0.5ms，快速连续选号时所有 lastUsed 完全相等，Before 全 false 会恒选
		// candsAll[0] 导致集中。usedSeq 严格全序，与时间精度无关。
		e = candsAll[0]
		for _, c := range candsAll[1:] {
			if c.usedSeq < e.usedSeq {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible) // eligible 保序 = top5 降序子集，权重直接用预计算值
	}
	e.lastUsed = now // 锁内即时标记：下一个进入 pick 的 goroutine 立即看到本号已用
	p.pickSeq++
	e.usedSeq = p.pickSeq // 单调序号：保证 usedSeq 严格全序（防惊群/LRU 的权威依据）
	p.notePickLocked(e, now)
	return e.a
}

// notePickLocked 速率调度记账（防"单号被本地调度打太多"）：窗口内被选中达到上限时
// 置 CoolRate 冷却，冷却期内该号不参与选号（也不参与全冷却兜底探测，见
// pickEarliestExpiryLocked）——把负载摊给其它号，而不是把某一个号刷成风控样本。
//
// 记账范围：normal 选号（pick）与粘性命中（PickByUIDForModel）都算"一次选中"；
// 兜底探测（pickEarliestExpiryLocked）不记账——它本来就是"无健康号"时的降级，
// 且速率冷却号已被排除在兜底之外，不会形成自激。
//
// 窗口语义（配额制，非滑动窗）：一个窗口内累计 rateLimit 次选中即冷却 rateCooldown，
// **打满时窗口一并清零**——解冻后从零开始重新攒配额。若不清零，解冻时窗口往往尚未
// 过期（rateCooldown < rateWindow 时必然如此），hotCount 仍是 rateLimit，解冻后第一次
// 被选中就立刻再次打满 → 实际锁定时长变成"冷却 + 等到窗口过期"（最坏 rateCooldown +
// rateWindow），配置给出的容量根本兑现不了。2026-09-29 的池子被打空即由此放大。
// 调用方必须已持有 p.mu 写锁。
func (p *Pool) notePickLocked(e *entry, now time.Time) {
	if p.rateLimit <= 0 || p.rateWindow <= 0 || p.rateCooldown <= 0 {
		return
	}
	if e.hotWindowStart.IsZero() || now.Sub(e.hotWindowStart) >= p.rateWindow {
		e.hotWindowStart = now
		e.hotCount = 0
	}
	e.hotCount++
	if e.hotCount < p.rateLimit {
		return
	}
	e.until = now.Add(p.rateCooldown)
	e.coolKind = CoolRate
	e.reason = fmt.Sprintf("rate schedule: picked %d times within %s, cooled %s", p.rateLimit, p.rateWindow, p.rateCooldown)
	e.hotWindowStart = time.Time{} // 打满即清窗：解冻后重新攒配额（见上方"窗口语义"）
	e.hotCount = 0
	p.dirty.Store(true)
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）排除——调了必 402，浪费轮换并产生噪音日志；
// CoolRate（本地速率冷却）同样排除——兜底探测会立刻给"刚被打太多"的号再加一次上游调用，
// 与限速目标相反（宁可快速 503，让 failgate 退避）。
// reqModel 对该模型处于冷却（6004 配额 / 11102 负缓存）的账号同样排除——上游已给出权威
// 恢复时刻，提前探测必然失败，只会白烧配额并给风控送样本。**注意这与"模型豁免"是两回事**：
// 模型豁免作用在 normal 选号（healthyForModel 让该号对其它模型仍可用），兜底这里排的是
// "本请求这个模型上明确被上游拒过"的号。
// 实现提示：模型级冷却只写 modelCooldowns、不写 e.until，故下方 expiry() 为零值也会跳过；
// 但**账号级冷却 + 模型级冷却并存**时 expiry() 非零，必须靠本显式判断拦住（否则照样撞 6004）。
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm, reqModel string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.disabled {
			continue // 禁用的账号永不参与兜底
		}
		if (e.coolKind == CoolHard || e.coolKind == CoolRate) && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（调了必 402）与速率冷却号（本地限速）不参与兜底
		}
		if e.modelCooled(now, reqModel) {
			continue // 该模型被上游拒过且未到恢复时刻：探测必失败
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry uid=%s until=%s kind=%s", logfmt.UID8(best.a.UID), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// 兜底选中同样推进 usedSeq/pickSeq（上游 49930b2）：entry.usedSeq 的契约是
	// 「每次被选中时取 pickSeq 自增值」，兜底也是选中；漏推进会让刚被兜底用过的
	// 账号 usedSeq 恒 0，在 pick 的 LRU 兜底（按 usedSeq 取最旧）眼里仍是最旧，
	// 被立刻重选——防集中/防惊群失效，且与本函数已更新 lastUsed 的事实自相矛盾。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（max=0 不限 → 恒 false）。
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读上限字段）。上限按 realm
// 分档（global 档 maxInFlightGlobal，WAF 403 修复 P1-1；未设置回落 maxInFlight）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// weighted 单个候选的选号预计算结果：权重（weightOf，每候选一次）+ 成本分层
// （tier/cost1k，每候选一次）。sort 比较器与 pickWeighted 抽签都只读缓存字段，
// 任何一处现算（旧实现在比较器内重算 costTier、在 pickWeighted 内重算 weightOf）
// 都会翻成 O(n log n) 次冗余浮点/map 查找，且引入两阶段口径分裂。
type weighted struct {
	e      *entry
	w      float64
	tier   int     // costTier 结果缓存（0 免费 / 1 无观测 / 2 收费）
	cost1k float64 // CostPer1k 缓存（tier 2 排序用；tier 0/1 恒 0）
}

// expiringWeight 快过期积分占比的权重系数（三因子之外的第四因子）。
// 取 8：略低于 credits 总量项（×10），足以在"快过期多"与"总量相近"的号之间拉开差距，
// 又不至于压过总量项让"总量大但快过期少"的号被完全饿死。
const expiringWeight = 8.0

// pickWeighted 三因子加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + idleWeight + successRate × 3
//
//	  - credits 比例 = 该号 credits / 全集最大 credits（避免量纲爆炸；全集口径：
//	    tier 过滤前的全部 healthy 候选，与截断排序共享基准——见 weighted 预计算注释）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//	  - successRate = successEMA/(successEMA+errorEMA)（EMA 衰减口径）；
//	    无请求记录给 1.5（中性偏信任）
//
// credits 全 0 时仍按 idle+successRate 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
// 候选携带调用方预计算的权重（weighted.w，maxCredits/now 均已按全集口径算好），
// 本函数**不再调用 weightOf**——单次 pick 内每个候选的权重只算一次，预计算与抽签
// 共用同一数值（旧实现在 eligible 子集上用子集 maxCredits 重算第二遍，两次口径
// 分裂：全集最大 credits 号被 minPickGap 挤出后，子集内 credits 比例整体膨胀）。
func (p *Pool) pickWeighted(cands []weighted) *entry {
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, c := range cands {
		// 四舍五入并保底权重 ≥1：向零截断会让 w<1/scale 的低权重号权重归零，
		// 彻底失去被抽中机会（候选少时加剧选号集中，惊群测试的放大因子之一）。
		// 保底 ≥1 同时保证 total ≥ len(cands) > 0（抽签分支无需 total<=0 兜底）。
		wi := int64(c.w*scale + 0.5)
		if wi < 1 {
			wi = 1
		}
		weights[i] = wi
		total += wi
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	r := rnd(total)
	var acc int64
	for i := range cands {
		acc += weights[i]
		if r < acc {
			return cands[i].e
		}
	}
	return cands[len(cands)-1].e
}

// weightEpsilon 等权重判定的浮点容差：权重公式微调（如 EMA 引入）后权重不再
// 位级相等，精确相等比较会让等权重洗牌静默失效、回到字典序截断饿死问题。
const weightEpsilon = 1e-9

// weightOf 计算单个账号的选号权重。单次 pick 内对每个候选只调用一次
// （预计算存入 weighted.w，sort 比较器与 pickWeighted 均读缓存不重算）。
// maxCredits / maxExpiring 均为**全集口径**（tier 过滤前的全部 healthy 候选），
// 保证截断排序与抽签权重共享同一基准。
func (p *Pool) weightOf(e *entry, maxCredits, maxExpiring int64, now time.Time) float64 {
	if p.weightOfHook != nil {
		p.weightOfHook() // DeptestOnly 观测：验证单次 pick 只算一次
	}
	if p.weightOfMaxHook != nil {
		p.weightOfMaxHook(maxCredits) // DeptestOnly 观测：验证全集口径
	}
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 1b. 快过期积分加成（issue:积分过期）：官方活动赠送的奖励积分按批过期，
	// 不用就作废。快过期积分多的号更该被选中，尽快把要作废的部分消耗掉。
	// 用**绝对量**（creditsExpiring / 全集 maxExpiring ×expiringWeight）而非占总量
	// 比例：占比会让"100 分里 90 分快过期"压过"10000 分里 5000 分快过期"，
	// 而后者要作废的绝对损失大 50 倍——目标是最小化作废积分绝对值。
	// 外层 expiringTier 已保证"有快过期积分的号整体优先"，本项在同层内排序。
	if e.creditsExpiring > 0 && maxExpiring > 0 {
		w += float64(e.creditsExpiring) / float64(maxExpiring) * expiringWeight
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3. 成功率 ×3（EMA 衰减口径）：successEMA/(successEMA+errorEMA) 让近期行为
	// 主导——旧终身累计口径下历史错误是分母的永久部分，上游修复后权重永久回不来。
	// 无请求记录（两 EMA 均零）时给 1.5（中性偏信任，同旧口径）。
	if obs := e.successEMA + e.errorEMA; obs > 0 {
		w += e.successEMA / obs * 3
	} else {
		w += 1.5 // 无请求记录 → 中性偏信任
	}
	return w
}

// SetCredits 更新账号余额。
