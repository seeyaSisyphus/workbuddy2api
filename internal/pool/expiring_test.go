package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestExpiringCreditBoostsWeight 快过期积分多的号权重大于少的号（同总量下）。
// 权重项用绝对量（creditsExpiring / 全集 maxExpiring），不再用占总量比例。
func TestExpiringCreditBoostsWeight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "expiring-heavy"})          // 总量相同,快过期多
	p.Add(&auth.Auth{UID: "stable-heavy"})            // 总量相同,快过期少
	p.SetCreditsDetailed("expiring-heavy", 1000, 900) // 900 快过期
	p.SetCreditsDetailed("stable-heavy", 1000, 50)    // 50 快过期

	p.mu.RLock()
	eExp := p.byUID["expiring-heavy"]
	eSta := p.byUID["stable-heavy"]
	now := time.Now()
	wExp := p.weightOf(eExp, 1000, 900, now)
	wSta := p.weightOf(eSta, 1000, 900, now)
	p.mu.RUnlock()

	if wExp <= wSta {
		t.Errorf("expiring-heavy weight %.3f <= stable-heavy %.3f; 快过期积分多者应加权", wExp, wSta)
	}
	// 差值应约等于 (900-50)/900*expiringWeight = 0.9444*8 ≈ 7.56
	diff := wExp - wSta
	if diff < 7.0 || diff > 8.0 {
		t.Errorf("weight diff %.3f, want ~7.56 ((900-50)/900*expiringWeight)", diff)
	}
}

// TestExpiringAbsoluteBeatsRatio 绝对量口径：快过期积分多的号胜过"占比高但绝对量少"的号。
// 旧占比口径下 "100 分里 90 分快过期" 会压过 "10000 分里 5000 分快过期"，
// 但后者要作废的绝对损失大 50 倍。选号目标是让快作废的积分尽量被用掉。
func TestExpiringAbsoluteBeatsRatio(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "small-ratio-high"}) // 100 里 90 快过期（占比 90%）
	p.Add(&auth.Auth{UID: "big-ratio-low"})    // 10000 里 5000 快过期（占比 50%）
	p.SetCreditsDetailed("small-ratio-high", 100, 90)
	p.SetCreditsDetailed("big-ratio-low", 10000, 5000)

	p.mu.RLock()
	now := time.Now()
	wSmall := p.weightOf(p.byUID["small-ratio-high"], 10000, 5000, now)
	wBig := p.weightOf(p.byUID["big-ratio-low"], 10000, 5000, now)
	p.mu.RUnlock()

	if wBig <= wSmall {
		t.Errorf("绝对量口径下 big(5000 快过期) 应压过 small(90 快过期): big=%.3f small=%.3f", wBig, wSmall)
	}
}

// TestExpiringOutranksCreditCount 用户需求的核心验收：快过期积分优先于积分总量。
// 低总量但含快过期积分的号，必须压过高总量但无快过期积分的号。
func TestExpiringOutranksCreditCount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "expiring-small"}) // 总量小，但 100 分即将作废
	p.Add(&auth.Auth{UID: "stable-huge"})    // 总量巨大，但无快过期积分
	p.SetCreditsDetailed("expiring-small", 100, 100)
	p.SetCreditsDetailed("stable-huge", 100000, 0)

	for i := range 50 {
		a := p.Pick("")
		if a == nil || a.UID != "expiring-small" {
			t.Fatalf("第 %d 次选中 %v，want expiring-small（快过期积分必须优先于积分总量）", i, a)
		}
	}
}

// TestExpiringTierClearsWhenExhausted 快过期积分清零后，选号回落到按积分总量的常规口径。
func TestExpiringTierClearsWhenExhausted(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "was-expiring"})
	p.Add(&auth.Auth{UID: "stable-huge"})
	p.SetCreditsDetailed("was-expiring", 100, 100)
	p.SetCreditsDetailed("stable-huge", 100000, 0)

	if a := p.Pick(""); a == nil || a.UID != "was-expiring" {
		t.Fatalf("pick=%v want was-expiring while it has expiring credits", a)
	}
	// 快过期部分被消耗完（NoteModelCost 递减 / 刷新回写）→ 不再特殊优先。
	p.SetCreditsDetailed("was-expiring", 100, 0)
	for i := range 20 {
		if a := p.Pick(""); a == nil || a.UID != "stable-huge" {
			t.Fatalf("第 %d 次选中 %v，want stable-huge（快过期清零后按总量优先）", i, a)
		}
	}
}

// TestExpiringTierDoesNotExcludeCostFree 快过期分层与成本分层正交：
// 有快过期积分的收费号，不应因"无快过期积分的免费号"而整体被挤出。
func TestExpiringTierDoesNotExcludeCostFree(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "expiring-paid"})
	p.Add(&auth.Auth{UID: "stable-free"})
	p.SetCreditsDetailed("expiring-paid", 500, 500)
	p.SetCreditsDetailed("stable-free", 500, 0)
	p.NoteModelCost("expiring-paid", "hy4-preview", 2.9, 1000) // 收费
	p.NoteModelCost("stable-free", "hy4-preview", 0, 1000)     // 免费

	// 快过期优先 > 成本免费：消耗快过期积分是首要目标，否则那部分积分直接作废。
	for i := range 30 {
		a := p.PickExcludingForRealm(nil, "hy4-preview", "")
		if a == nil || a.UID != "expiring-paid" {
			t.Fatalf("第 %d 次选中 %v，want expiring-paid（快过期优先于成本免费）", i, a)
		}
	}
}

// TestExpiringClampedToCredits expiring 超过总量/负值时被钳制,不污染权重。
func TestExpiringClampedToCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 9999) // expiring > credits
	p.mu.RLock()
	if p.byUID["u1"].creditsExpiring != 100 {
		t.Errorf("creditsExpiring=%d want 100 (clamped to credits)", p.byUID["u1"].creditsExpiring)
	}
	p.mu.RUnlock()

	p.SetCreditsDetailed("u1", 100, -5) // 负值
	p.mu.RLock()
	if p.byUID["u1"].creditsExpiring != 0 {
		t.Errorf("creditsExpiring=%d want 0 (negative clamped)", p.byUID["u1"].creditsExpiring)
	}
	p.mu.RUnlock()
}

// TestSetCreditsLeavesExpiringUnchanged 旧 SetCredits 只更新总量,不清 expiring(向后兼容)。
func TestSetCreditsLeavesExpiringUnchanged(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 500, 200)
	p.SetCredits("u1", 600) // 旧入口只更新总量
	p.mu.RLock()
	e := p.byUID["u1"]
	if e.credits != 600 {
		t.Errorf("credits=%d want 600", e.credits)
	}
	if e.creditsExpiring != 200 {
		t.Errorf("creditsExpiring=%d want 200 (SetCredits 不应清)", e.creditsExpiring)
	}
	p.mu.RUnlock()
}
