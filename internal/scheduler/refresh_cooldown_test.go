package scheduler

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestCreditRefreshKeepsModelCooling 定期余额刷新是只读探测，不得清掉 6004 模型级
// 冷却（旧实现经 ReenableIfCredits 全量解冻，每 10 分钟抹一次 → 已限流的号被反复
// 重打成 429/风控样本）；同时余额仍要正常回写。
func TestCreditRefreshKeepsModelCooling(t *testing.T) {
	srv, _ := newCheckinServer(t)
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(2*time.Hour), "deepseek-v4.1-flash", "6004 model rate limit")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunCreditRefreshNow()

	if got := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", ""); got != nil {
		t.Fatalf("6004 model cooldown must survive credit refresh, picked %s", got.UID)
	}
	if st, _ := p.Status("u1"); st.Credits != 999 {
		t.Errorf("credits=%d want 999 (refresh must still write balance)", st.Credits)
	}
}
