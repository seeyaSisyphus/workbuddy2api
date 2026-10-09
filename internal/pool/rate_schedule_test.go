package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestRateScheduleCoolsAfterLimit 窗口内被选中达到上限 → 置 CoolRate 冷却；
// 冷却号不参与全冷却兜底（不��探测刚被打多的号，宁可快速失败）。
func TestRateScheduleCoolsAfterLimit(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetRateSchedule(15*time.Minute, 3, 30*time.Minute)

	for i := 0; i < 3; i++ {
		if got := p.Pick(""); got == nil {
			t.Fatalf("pick %d: nil", i+1)
		}
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "rate_schedule" {
		t.Fatalf("after limit: want cooling kind=rate_schedule, got %+v", st)
	}
	if got := p.Pick(""); got != nil {
		t.Fatalf("rate-cooled account must not be probed by fallback, got %s", got.UID)
	}
}

// TestRateScheduleRecoversAfterWindow 冷却与窗口都滑过后恢复可用（自愈，不需要人工干预）。
func TestRateScheduleRecoversAfterWindow(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetRateSchedule(60*time.Millisecond, 1, 20*time.Millisecond)

	if got := p.Pick(""); got == nil {
		t.Fatal("first pick should succeed")
	}
	if got := p.Pick(""); got != nil {
		t.Fatal("second pick within window should be blocked (rate cooled)")
	}
	time.Sleep(120 * time.Millisecond) // 冷却(20ms)与窗口(60ms)均已滑过
	if got := p.Pick(""); got == nil {
		t.Fatal("should recover after window + cooldown")
	}
}

// TestRateScheduleResetsWindowOnCool 打满冷却时窗口必须一并清零：解冻后应拿到**全新配额**，
// 而不是"第一次被选中就立刻再次打满"。
//
// 回归场景（2026-09-29 池子被打空）：rateCooldown(30ms) < rateWindow(1s)，解冻时旧窗口
// 远未过期。旧实现不清 hotCount，解冻后第一个 pick 立刻把 hotCount 顶到 rateLimit →
// 再次冷却，账号被"冷却 + 等窗口过期"双重锁住，配置容量根本兑现不了。
func TestRateScheduleResetsWindowOnCool(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetRateSchedule(time.Second, 2, 30*time.Millisecond)

	for i := 0; i < 2; i++ {
		if got := p.Pick(""); got == nil {
			t.Fatalf("pick %d: nil", i+1)
		}
	}
	if got := p.Pick(""); got != nil {
		t.Fatal("should be rate cooled after limit")
	}
	time.Sleep(40 * time.Millisecond) // 冷却到期，但窗口(1s)远未过期
	if got := p.Pick(""); got == nil {
		t.Fatal("解冻后第一个 pick 必须成功（旧实现：hotCount 未清零 → 解冻即再冷却）")
	}
	if got := p.Pick(""); got == nil {
		t.Fatal("解冻后第二个 pick 仍在全新配额内，必须成功")
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "rate_schedule" {
		t.Fatalf("新配额用尽后应再次冷却，got %+v", st)
	}
}

// TestRateScheduleDisabledByDefault 未注入参数 = 关闭（零回归）。
func TestRateScheduleDisabledByDefault(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 50; i++ {
		if got := p.Pick(""); got == nil {
			t.Fatalf("pick %d: nil（未注入速率调度不应冷却）", i+1)
		}
	}
}

// TestRateScheduleCountsStickyPicks 粘性命中（PickByUIDForModel）同样计入速率窗口：
// 会话被钉住的号不能绕过限速（否则"单号独食"形态照样发生）。
func TestRateScheduleCountsStickyPicks(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetRateSchedule(15*time.Minute, 3, 30*time.Minute)

	for i := 0; i < 3; i++ {
		if a := p.PickByUIDForModel("u1", "m"); a == nil {
			t.Fatalf("sticky pick %d: nil", i+1)
		}
	}
	if a := p.PickByUIDForModel("u1", "m"); a != nil {
		t.Error("sticky path must be rate-limited after the window limit (handler 借此解绑轮换)")
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "rate_schedule" {
		t.Fatalf("want rate_schedule cooling, got %+v", st)
	}
}
