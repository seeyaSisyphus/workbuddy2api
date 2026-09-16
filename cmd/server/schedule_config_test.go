package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadScheduleWindowAndRefresh 新增排程键的加载与解析：
// checkin_window 解析为偏移量并置 HasWindow，jitter/credit_refresh 解析为 Duration，
// reasoning_alias 默认 true。
func TestLoadScheduleWindowAndRefresh(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"listen": ":9999",
		"schedule": {
			"checkin_window": "08:00-08:30",
			"checkin_jitter": "45s",
			"credit_refresh": "10m"
		}
	}`), 0o600)

	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.HasWindow {
		t.Fatal("HasWindow must be true")
	}
	if c.Schedule.WindowFromDur != 8*time.Hour {
		t.Errorf("window from=%v want 8h", c.Schedule.WindowFromDur)
	}
	if c.Schedule.WindowToDur != 8*time.Hour+30*time.Minute {
		t.Errorf("window to=%v want 8h30m", c.Schedule.WindowToDur)
	}
	if c.Schedule.CheckinJitterDur != 45*time.Second {
		t.Errorf("jitter=%v want 45s", c.Schedule.CheckinJitterDur)
	}
	if c.Schedule.CreditRefreshDur != 10*time.Minute {
		t.Errorf("credit_refresh=%v want 10m", c.Schedule.CreditRefreshDur)
	}
	if !c.Features.ReasoningAlias {
		t.Error("reasoning_alias must default true")
	}
}

// TestLoadRejectsBadWindow 非法窗口启动即报错（fail fast，不静默回落整点排程）。
func TestLoadRejectsBadWindow(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_window":"23:30-00:30"}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("cross-midnight window must be rejected")
	}
}

// TestLoadRejectsBadCreditRefresh 非法 credit_refresh 报错而非静默关闭。
func TestLoadRejectsBadCreditRefresh(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"credit_refresh":"nope"}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("bad credit_refresh must be rejected")
	}
}

// TestCreditRefreshDisabledByDefault 缺省不启用积分刷新（零改动现状）；
// 显式 "0" 同样是关闭语义。
func TestCreditRefreshDisabledByDefault(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CreditRefreshDur != 0 {
		t.Errorf("default credit_refresh=%v want 0 (disabled)", c.Schedule.CreditRefreshDur)
	}
	if c.Schedule.HasWindow {
		t.Error("default checkin_window must be disabled")
	}
}

// TestEnvScheduleOverrides WB2A_CHECKIN_WINDOW / WB2A_CREDIT_REFRESH /
// WB2A_REASONING_ALIAS 环境变量覆盖生效。
func TestEnvScheduleOverrides(t *testing.T) {
	t.Setenv("WB2A_CHECKIN_WINDOW", "09:15-09:45")
	t.Setenv("WB2A_CREDIT_REFRESH", "30s")
	t.Setenv("WB2A_REASONING_ALIAS", "false")

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.HasWindow || c.Schedule.WindowFromDur != 9*time.Hour+15*time.Minute {
		t.Errorf("window=%v..%v on=%v want 9h15m..9h45m",
			c.Schedule.WindowFromDur, c.Schedule.WindowToDur, c.Schedule.HasWindow)
	}
	if c.Schedule.CreditRefreshDur != 30*time.Second {
		t.Errorf("credit_refresh=%v want 30s", c.Schedule.CreditRefreshDur)
	}
	if c.Features.ReasoningAlias {
		t.Error("reasoning_alias=false must disable the alias field")
	}
}
