package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRateScheduleDefaults 速率调度默认值：15m 窗口 / 30 次上限 / 30m 冷却。
func TestRateScheduleDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.RateLimit != 30 {
		t.Errorf("rate_limit=%d want 30", c.Pool.RateLimit)
	}
	if c.RateWindowDur != 15*time.Minute {
		t.Errorf("rate_window=%v want 15m", c.RateWindowDur)
	}
	if c.RateCooldownDur != 30*time.Minute {
		t.Errorf("rate_cooldown=%v want 30m", c.RateCooldownDur)
	}
}

// TestRateScheduleFromFile 显式配置生效；limit<=0 = 关闭（池侧不注入即零回归）。
func TestRateScheduleFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"rate_window":"5m","rate_limit":7,"rate_cooldown":"10m"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.RateLimit != 7 || c.RateWindowDur != 5*time.Minute || c.RateCooldownDur != 10*time.Minute {
		t.Errorf("rate schedule=%d/%v/%v want 7/5m/10m", c.Pool.RateLimit, c.RateWindowDur, c.RateCooldownDur)
	}
}
