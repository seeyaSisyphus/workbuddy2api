package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestNextUsableInHealthy 健康池 → 立即可用（0, true）。
func TestNextUsableInHealthy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if d, ok := p.NextUsableIn("m", ""); !ok || d != 0 {
		t.Fatalf("healthy → want (0,true), got (%v,%v)", d, ok)
	}
}

// TestNextUsableInModelCooling 6004 模型级冷却 → 返回该模型冷却剩余；别的模型不受影响。
func TestNextUsableInModelCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "m", "6004 model rate limit")

	// 上界含 resetSkewBuffer：权威重置的对齐截止 = reset + 90s（见 cappedResetUntilLocked）。
	if d, ok := p.NextUsableIn("m", ""); !ok || d < 55*time.Minute || d > time.Hour+resetSkewBuffer+time.Minute {
		t.Fatalf("model cooling → want ~1h, got (%v,%v)", d, ok)
	}
	if d, ok := p.NextUsableIn("other", ""); !ok || d != 0 {
		t.Fatalf("model exemption: other model should be usable now, got (%v,%v)", d, ok)
	}
}

// TestNextUsableInTakesLatestDeadline 账号级冷却与该模型冷��同时存在 → 取较晚者
// （要同时越过所有截止才算恢复）���
func TestNextUsableInTakesLatestDeadline(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Now().Add(10*time.Minute), "429 rate limit")
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(30*time.Minute), "m", "6004 model rate limit")

	d, ok := p.NextUsableIn("m", "")
	if !ok || d < 25*time.Minute || d > 30*time.Minute+resetSkewBuffer+time.Minute {
		t.Fatalf("want latest deadline ~30m, got (%v,%v)", d, ok)
	}
}

// TestNextUsableInNoAccount 无非禁用账号 → ok=false（等多久都没用）。
func TestNextUsableInNoAccount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if _, ok := p.NextUsableIn("m", ""); ok {
		t.Fatal("no non-disabled account → want ok=false")
	}
}

// TestNextUsableInRealmFilter realm 谓词：只算本域账号的恢复时间。
func TestNextUsableInRealmFilter(t *testing.T) {
	p := realmPool(t)
	for _, uid := range []string{"g1", "g2"} {
		p.CooldownSoftForModel(uid, 600*time.Second, time.Now().Add(time.Hour), "m", "6004 model rate limit")
	}
	if d, ok := p.NextUsableIn("m", "global"); !ok || d < 55*time.Minute || d > time.Hour+resetSkewBuffer+time.Minute {
		t.Fatalf("global → want ~1h, got (%v,%v)", d, ok)
	}
	if d, ok := p.NextUsableIn("m", "cn"); !ok || d != 0 {
		t.Fatalf("cn → want 0, got (%v,%v)", d, ok)
	}
}
