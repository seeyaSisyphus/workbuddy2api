package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestLowCreditStopFiltersBottomLine 第 0 层底线：credits 低于 low_credit_stop 的候选被跳过，
// 余额充足的号独拿全部选中。
func TestLowCreditStopFiltersBottomLine(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetLowCredit(100, 300)
	p.Add(&auth.Auth{UID: "low"})
	p.Add(&auth.Auth{UID: "high"})
	p.SetCredits("low", 50)
	p.SetCredits("high", 5000)
	for i := range 30 {
		if a := p.Pick(""); a == nil || a.UID != "high" {
			t.Fatalf("第 %d 次选中 %v，want high（低于底线的号必须被跳过）", i, a)
		}
	}
	st, _ := p.Status("low")
	if !st.LowBalance {
		t.Errorf("low 的 low_balance 应为 true（credits=50 < stop=100）")
	}
	st, _ = p.Status("high")
	if st.LowBalance {
		t.Errorf("high 的 low_balance 应为 false")
	}
}

// TestLowCreditStopExpiringExempt 豁免：credits 低于底线但 creditsExpiring >= stop 的号不过滤
// （快过期的分不用白不用）。正常数据不变式 expiring<=credits 下豁免不触发，这里直接构造
// 不一致状态（如旧 state.json 未钳制）验证豁免分支本身生效。
func TestLowCreditStopExpiringExempt(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetLowCredit(100, 300)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "low-expiring"})
	p.Add(&auth.Auth{UID: "high"})
	p.SetCredits("low-expiring", 50)
	p.SetCredits("high", 5000)
	p.mu.Lock()
	p.byUID["low-expiring"].creditsExpiring = 200 // 不一致数据：expiring >= stop 但 credits < stop
	p.mu.Unlock()

	// 豁免生效 → low-expiring 留在候选集；快过期分层（expiringTier）再把它整体优先于 high。
	for i := range 30 {
		a := p.Pick("")
		if a == nil || a.UID != "low-expiring" {
			t.Fatalf("第 %d 次选中 %v，want low-expiring（快过期豁免 + 快过期分层优先）", i, a)
		}
	}
}

// TestLowCreditStopDegradesWhenAllLow 降级：全部候选都低于底线且无豁免时，跳过底线层
// 照常选号（不为保护机制牺牲可用性）。
func TestLowCreditStopDegradesWhenAllLow(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetLowCredit(100, 300)
	p.Add(&auth.Auth{UID: "low-a"})
	p.Add(&auth.Auth{UID: "low-b"})
	p.SetCredits("low-a", 10)
	p.SetCredits("low-b", 20)
	for i := range 20 {
		if a := p.Pick(""); a == nil {
			t.Fatalf("第 %d 次选中 nil，want 降级后仍可选出低余额号", i)
		}
	}
}

// TestLowCreditStopDisabled 底线关闭（0）：低余额号照常参与选号。
func TestLowCreditStopDisabled(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetLowCredit(0, 300)
	p.Add(&auth.Auth{UID: "low"})
	p.SetCredits("low", 1)
	if a := p.Pick(""); a == nil || a.UID != "low" {
		t.Fatalf("pick=%v want low（low_credit_stop=0 时不过滤）", a)
	}
}

// TestNoteModelCostLowCreditProbeOnCross 事件探测：NoteModelCost 扣费使 credits 下行
// 跨越 low_credit_accel 时触发一次回调；跌破后再次扣减不重复触发（边缘触发）；
// 余额回满后再次跌破重新触发。回调在锁外同步调用（延迟由 scheduler 侧负责）。
func TestNoteModelCostLowCreditProbeOnCross(t *testing.T) {
	p := New("")
	p.SetLowCredit(100, 300)
	var probed []string
	p.SetLowCreditProbeFunc(func(uid string) { probed = append(probed, uid) })
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 310)

	p.NoteModelCost("u1", "m", 20, 1000) // 310 → 290：跌破 300
	if len(probed) != 1 || probed[0] != "u1" {
		t.Fatalf("跌破阈值应触发一次探测回调，got %v", probed)
	}
	p.NoteModelCost("u1", "m", 20, 1000) // 290 → 270：已在阈值之下，不重复触发
	if len(probed) != 1 {
		t.Fatalf("阈值之下再次扣减不应重复触发，got %v", probed)
	}
	p.SetCredits("u1", 500)              // 探测回写/签到回满
	p.NoteModelCost("u1", "m", 0, 1000)  // 免费请求不动余额、不触发
	p.NoteModelCost("u1", "m", 250, 1000) // 500 → 250：再次跌破
	if len(probed) != 2 {
		t.Fatalf("回满后再次跌破应重新触发，got %v", probed)
	}
}

// TestLowCreditActiveUIDs 加速档活跃口径：低余额 + 近窗口内被选中过的非禁用号才列入。
func TestLowCreditActiveUIDs(t *testing.T) {
	p := New("")
	p.SetLowCredit(100, 300)
	p.Add(&auth.Auth{UID: "active-low"})
	p.Add(&auth.Auth{UID: "idle-low"})
	p.Add(&auth.Auth{UID: "high"})
	p.SetCredits("active-low", 200)
	p.SetCredits("idle-low", 100)
	p.SetCredits("high", 5000)
	// 近窗口被选中过 → 活跃。
	p.mu.Lock()
	p.byUID["active-low"].lastUsed = time.Now()
	p.byUID["idle-low"].lastUsed = time.Now().Add(-time.Hour) // 超出窗口
	p.mu.Unlock()

	got := p.LowCreditActiveUIDs(300, 10*time.Minute)
	if len(got) != 1 || got[0] != "active-low" {
		t.Fatalf("LowCreditActiveUIDs=%v want [active-low]", got)
	}
	if got := p.LowCreditActiveUIDs(0, 10*time.Minute); len(got) != 0 {
		t.Fatalf("threshold=0 应恒空，got %v", got)
	}
	p.Disable("active-low", "test")
	if got := p.LowCreditActiveUIDs(300, 10*time.Minute); len(got) != 0 {
		t.Fatalf("禁用后不应列入，got %v", got)
	}
}

// TestLowCreditStopFreeModelExempt 限免豁免：低于底线的号对请求模型持有有效「实测免费」
// 观测（成本账本 tier 0）时不过滤——该模型请求不耗积分，底线前提不成立。
// 豁免后 tier 0 在成本分层中整体优先，免费号独拿该模型的全部选中。
func TestLowCreditStopFreeModelExempt(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetLowCredit(100, 300)
	p.Add(&auth.Auth{UID: "low-free"})
	p.Add(&auth.Auth{UID: "high"})
	p.SetCredits("low-free", 50)
	p.SetCredits("high", 5000)
	p.NoteModelCost("low-free", "ds-flash", 0, 1000) // 实测免费观测（credit=0 不动余额）

	for i := range 30 {
		if a := p.Pick("ds-flash"); a == nil || a.UID != "low-free" {
			t.Fatalf("第 %d 次选中 %v，want low-free（限免豁免 + tier0 整体优先）", i, a)
		}
	}
}

// TestLowCreditStopFreeExemptPerModel 豁免按（账号, 模型）各自判定：对其他无免费观测的
// 模型，低余额号仍被底线过滤；观测过期（>modelCostTTL）后豁免同样失效。
func TestLowCreditStopFreeExemptPerModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetLowCredit(100, 300)
	p.Add(&auth.Auth{UID: "low-free"})
	p.Add(&auth.Auth{UID: "high"})
	p.SetCredits("low-free", 50)
	p.SetCredits("high", 5000)
	p.NoteModelCost("low-free", "ds-flash", 0, 1000) // 仅 ds-flash 上有免费观测

	// 其他模型：low-free 无免费观测 → 仍被底线过滤，high 独拿。
	for i := range 20 {
		if a := p.Pick("other"); a == nil || a.UID != "high" {
			t.Fatalf("第 %d 次选中 %v，want high（豁免不跨模型）", i, a)
		}
	}

	// 观测过期：把 LastSeen 拨到 TTL 之前 → ds-flash 上豁免失效，回到底线过滤。
	p.mu.Lock()
	mc := p.byUID["low-free"].modelCost["ds-flash"]
	mc.LastSeen = time.Now().Add(-modelCostTTL - time.Minute)
	p.byUID["low-free"].modelCost["ds-flash"] = mc
	p.mu.Unlock()
	for i := range 20 {
		if a := p.Pick("ds-flash"); a == nil || a.UID != "high" {
			t.Fatalf("观测过期后第 %d 次选中 %v，want high（豁免失效）", i, a)
		}
	}
}
