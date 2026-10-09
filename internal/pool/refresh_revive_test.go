package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestReviveIfCreditExhaustedKeepsModelCooling 定期余额探测只做定向解冻：
// 6004 模型级冷却必须存活——旧实现（探测走 ReenableIfCredits 全量解冻）每 10
// 分钟抹一次模型冷却，已限流的号被反复重打、持续给上游风控送样本。
func TestReviveIfCreditExhaustedKeepsModelCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(2*time.Hour), "deepseek-v4.1-flash", "6004 model rate limit")

	p.ReviveIfCreditExhausted("u1", 500)

	if got := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", ""); got != nil {
		t.Fatalf("model cooldown must survive credit probe, picked %s", got.UID)
	}
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("rate_limited_models=%+v want one row for deepseek-v4.1-flash", st.RateLimitedModels)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500（探测仍应回写��额）", st.Credits)
	}
}

// TestReviveIfCreditExhaustedKeepsSoftCooling 429/WAF 软冷却同样不被探测解冻。
func TestReviveIfCreditExhaustedKeepsSoftCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Now().Add(time.Hour), "429 rate limit")

	p.ReviveIfCreditExhausted("u1", 500)

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("soft cooling must survive credit probe")
	}
}

// TestReviveIfCreditExhaustedClearsHardCredit 余额恢复 → 定向解冻「余额耗尽」硬冷却。
func TestReviveIfCreditExhaustedClearsHardCredit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")

	p.ReviveIfCreditExhausted("u1", 500)

	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("hard-credit cooldown should be cleared, pick=%+v", got)
	}
}

// TestReviveIfCreditExhaustedNoopCases 余额仍为 0 / 禁用号：不解冻。
func TestReviveIfCreditExhaustedNoopCases(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReviveIfCreditExhausted("u1", 0)
	if st, _ := p.Status("u1"); !st.Cooling {
		t.Fatal("remain=0 must keep hard cooling")
	}

	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p2.Disable("u1", "session dead")
	p2.ReviveIfCreditExhausted("u1", 500)
	if p2.Pick("") != nil {
		t.Fatal("disabled account must not be revived by credit probe")
	}
}
