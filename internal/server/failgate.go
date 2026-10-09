// failgate.go 故障门（fail gate）：连续「不可服务」失败达到阈值后短暂开门，
// 直接以 429 + Retry-After 拒绝请求，把客户端重试风暴挡在选号/上游轮换之前。
//
// 背景（2026-09-27 重试风暴复盘）：全池冷却时网关"秒回 503"，客户端按秒空转重试
// （实测 13 分钟 600 次、平均间隔 1.38s），并反复触发上游轮换。单次失败仍是原
// 503 语义；只有**连续**失败才开门退避，任意一次成功即复位。
package server

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// gateFailThreshold 连续失败达到该次数后开门。取 5：正常偶发失败（单个号撞限流
// 但轮换成功、客户端重试一次就好）不会触发；只有"每个请求都失败"的风暴形态才开门。
const gateFailThreshold = 5

// gateWindow 连续失败计数窗口；窗口内未再失败则重新计数（避免把几小时前的失败
// 和现在的失败串成"连续"）。
const gateWindow = time.Minute

// gateBackoffBase / gateBackoffMax 开门退避：2s 起、每次开门翻倍、封顶 30s。
// 取小值是因为开门只是"让客户端歇口气"，不是惩罚；上游真正的恢复时刻由
// Retry-After（Pool.NextUsableIn）单独提示。
const (
	gateBackoffBase = 2 * time.Second
	gateBackoffMax  = 30 * time.Second
)

// chatGate 故障门：按 key（realm|model）隔离的连续失败计数与退避。
// 按模型隔离的原因：某模型的失败风暴不应误伤其它模型（例如 global 某模型全池
// 冷却时，CN 侧正常模型仍应可服务）。零值可用。
type chatGate struct {
	mu sync.Mutex
	m  map[string]*gateState
}

// gateState 单个 key 的故障门状态。
type gateState struct {
	mu         sync.Mutex
	fails      int       // 窗口内连续失败数
	windowFrom time.Time // 计数窗口起点
	until      time.Time // 开门截止（now < until 期间拒绝）
	backoff    time.Duration
}

// state 取得（必要时创建）key 对应的状态；调用方必须持有 g.mu。
func (g *chatGate) state(key string) *gateState {
	if g.m == nil {
		g.m = map[string]*gateState{}
	}
	st, ok := g.m[key]
	if !ok {
		st = &gateState{}
		g.m[key] = st
	}
	return st
}

// open 报告门是否打开；打开时返回剩余等待时长（>0）。
func (g *chatGate) open(key string, now time.Time) (time.Duration, bool) {
	g.mu.Lock()
	st := g.state(key)
	g.mu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if now.Before(st.until) {
		return st.until.Sub(now), true
	}
	return 0, false
}

// noteFail 记一次「不可服务」失败（无可用账号 / 全池软限流）。连续达到阈值即开门；
// hint 是池层给出的"最早可能恢复时长"，比本门退避更晚时对齐 hint（不提前放行）。
func (g *chatGate) noteFail(key string, now time.Time, hint time.Duration) {
	g.mu.Lock()
	st := g.state(key)
	g.mu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.windowFrom.IsZero() || now.Sub(st.windowFrom) > gateWindow {
		st.windowFrom = now
		st.fails = 0
	}
	st.fails++
	if st.fails < gateFailThreshold {
		return
	}
	if st.backoff == 0 {
		st.backoff = gateBackoffBase
	} else if st.backoff < gateBackoffMax {
		if st.backoff *= 2; st.backoff > gateBackoffMax {
			st.backoff = gateBackoffMax
		}
	}
	d := st.backoff
	if hint > d {
		d = hint
	}
	if d > gateBackoffMax {
		d = gateBackoffMax
	}
	st.until = now.Add(d)
}

// noteOK 该 key 任意一次成功即复位（失败计数、窗口与退避指数一起清）。
func (g *chatGate) noteOK(key string) {
	g.mu.Lock()
	st := g.state(key)
	g.mu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.fails = 0
	st.windowFrom = time.Time{}
	st.until = time.Time{}
	st.backoff = 0
}

// retryAfterSeconds 把等待时长换算成 Retry-After 头值：向上取整，钳 [1,60] 秒
// （HTTP 语义要求为非负整数秒；上限防止把客户端锁太久——恢复时刻可能因新账号/解锁提前）。
func retryAfterSeconds(d time.Duration) string {
	secs := int(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	if secs > 60 {
		secs = 60
	}
	return strconv.Itoa(secs)
}

// formatWait 把等待时长渲染成人读短标签（`30s` / `45m` / `2h20m`），供 503 文案
// 里的"最早恢复时长"用。与 retryAfterSeconds 分工不同：那个是 Retry-After **头**（钳
// 1~60s、机器读），这个是 **message** 里的人读时长，不钳位——把 2 小时说成 60 秒才是撒谎。
//
// 一律向上取整到展示单位（不早于真实恢复时刻）：宁可说"2h22m"也不说"2h20m"而让
// 客户端在恢复前 2 分钟又空转一轮。
func formatWait(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int((d+time.Second-1)/time.Second))
	case d < time.Hour:
		m := int((d + time.Minute - 1) / time.Minute)
		if m >= 60 { // 59m30s 这类向上取整到 60m → 进位成 1h，避免出现 "60m"
			return "1h"
		}
		return fmt.Sprintf("%dm", m)
	default:
		h := int(d / time.Hour)
		m := int(((d % time.Hour) + time.Minute - 1) / time.Minute)
		if m >= 60 { // 向上取整到 60m → 进位成整点，避免出现 "2h60m"
			h, m = h+1, 0
		}
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%02dm", h, m)
	}
}
