package config

import (
	"testing"
	"time"
)

// TestParseWindowAccepted 合法窗口解析为当日偏移量；"8:00" 这类缺前导零写法同样接受。
func TestParseWindowAccepted(t *testing.T) {
	s := &Schedule{CheckinWindow: "08:00-08:30"}
	if err := s.ParseWindow(); err != nil {
		t.Fatalf("ParseWindow: %v", err)
	}
	if !s.HasWindow {
		t.Fatal("HasWindow must be true for non-empty window")
	}
	if s.WindowFromDur != 8*time.Hour {
		t.Errorf("from=%v want 8h", s.WindowFromDur)
	}
	if s.WindowToDur != 8*time.Hour+30*time.Minute {
		t.Errorf("to=%v want 8h30m", s.WindowToDur)
	}

	// 缺前导零也接受（用户手写配置的常见形态）。
	s2 := &Schedule{CheckinWindow: "8:00-9:30"}
	if err := s2.ParseWindow(); err != nil {
		t.Fatalf("ParseWindow(no leading zero): %v", err)
	}
	if s2.WindowFromDur != 8*time.Hour || s2.WindowToDur != 9*time.Hour+30*time.Minute {
		t.Errorf("offsets=%v..%v want 8h..9h30m", s2.WindowFromDur, s2.WindowToDur)
	}
}

// TestParseWindowEmptyDisables 空窗口不启用（回落 checkin_hours 整点排程）。
func TestParseWindowEmptyDisables(t *testing.T) {
	s := &Schedule{}
	if err := s.ParseWindow(); err != nil {
		t.Fatalf("ParseWindow(empty): %v", err)
	}
	if s.HasWindow {
		t.Error("HasWindow must be false for empty window")
	}
}

// TestParseWindowRejectsInvalid 非法窗口一律报错而非静默忽略：
// 静默忽略会让用户以为窗口生效、实际仍按整点打上游，是最难排查的一类配置 bug。
// 跨零点窗口（结束 <= 开始）显式拒绝——本实现的窗口只落在同一自然日内。
func TestParseWindowRejectsInvalid(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
	}{
		{"缺少分隔符", "08:00"},
		{"三段", "08:00-09:00-10:00"},
		{"小时越界", "25:00-26:00"},
		{"分钟越界", "08:60-09:00"},
		{"非数字", "aa:00-09:00"},
		{"结束等于开始", "08:00-08:00"},
		{"结束早于开始", "09:00-08:00"},
		{"跨零点", "23:30-00:30"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Schedule{CheckinWindow: c.in}
			if err := s.ParseWindow(); err == nil {
				t.Errorf("window %q must be rejected", c.in)
			}
			if s.HasWindow {
				t.Errorf("invalid window must not set HasWindow (%q)", c.in)
			}
		})
	}
}
