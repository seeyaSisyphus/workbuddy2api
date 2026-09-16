package scheduler

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/history"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestWindowTargetDerivesFromDate 窗口目标时刻由日期派生：同一天内多次计算一致
// （进程重启不改点），且落在 [from,to) 内。
func TestWindowTargetDerivesFromDate(t *testing.T) {
	s := New(Config{
		CheckinWindowOn:   true,
		CheckinWindowFrom: 8 * time.Hour,
		CheckinWindowTo:   8*time.Hour + 30*time.Minute,
	})
	base := time.Date(2026, 9, 16, 0, 0, 0, 0, time.Local)

	// 当天窗口未过：目标落在当天窗口内，且可重复计算得到同一时刻。
	now := base.Add(7 * time.Hour)
	got := s.windowTarget(now)
	if got.Before(base.Add(8*time.Hour)) || !got.Before(base.Add(8*time.Hour+30*time.Minute)) {
		t.Errorf("target=%v outside [08:00,08:30)", got)
	}
	if again := s.windowTarget(now); !again.Equal(got) {
		t.Errorf("target not stable within a day: %v vs %v", got, again)
	}

	// 当天窗口已过：顺延到明天窗口内。
	after := base.Add(9 * time.Hour)
	tomorrow := s.windowTarget(after)
	if !tomorrow.After(after) {
		t.Errorf("target=%v must be after now=%v", tomorrow, after)
	}
	if tomorrow.Day() != 17 {
		t.Errorf("expected next day, got %v", tomorrow)
	}
}

// TestWindowTargetVariesByDay 不同自然日取到不同时刻（日期种子参与，避免天天同点打上游）。
// 30 分钟窗口 = 1800 个秒位，连续两天撞同一秒位概率极低；这里断言"至少有一对不同"，
// 避免用固定日期硬编码期望值。
func TestWindowTargetVariesByDay(t *testing.T) {
	s := New(Config{
		CheckinWindowOn:   true,
		CheckinWindowFrom: 8 * time.Hour,
		CheckinWindowTo:   8*time.Hour + 30*time.Minute,
	})
	seen := map[int]bool{}
	for d := 0; d < 5; d++ {
		day := time.Date(2026, 9, 16+d, 0, 0, 0, 0, time.Local)
		seen[s.windowTarget(day.Add(7*time.Hour)).Second()] = true
	}
	if len(seen) < 2 {
		t.Errorf("targets identical across days: %v", seen)
	}
}

// TestNextWakeUsesWindowForCheckin 窗口开启时签到唤醒点走窗口，取代整点排程。
func TestNextWakeUsesWindowForCheckin(t *testing.T) {
	s := New(Config{
		CheckinWindowOn:   true,
		CheckinWindowFrom: 8 * time.Hour,
		CheckinWindowTo:   8*time.Hour + 30*time.Minute,
		// 其他任务全部禁用，保证 nextWake 只反映签到槽。
		TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
	})
	now := time.Date(2026, 9, 16, 7, 0, 0, 0, time.Local)
	next, kinds := s.nextWake(now)
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Fatalf("kinds=%v want [checkin]", kinds)
	}
	if h := next.Hour(); h != 8 {
		t.Errorf("hour=%d want 8 (window)", h)
	}
	if next.Minute() == 0 && next.Second() == 0 {
		t.Error("window checkin should not always land exactly at 08:00:00")
	}
}

// TestCheckinAllDoesNotJitter 手动入口不抖动：即便配了 jitter，CheckinAll 也应立即返回
// （交互式触发不该为反突发等待买单）。多账号 + 大 jitter 作为放大镜。
func TestCheckinAllDoesNotJitter(t *testing.T) {
	srv, _ := newCheckinServer(t)
	defer srv.Close()
	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	// jitter 设为 1 小时：若 CheckinAll 误用抖动，本测试会立即超时。
	s := New(Config{Pool: p, Upstream: up, CheckinJitter: time.Hour})

	done := make(chan struct{})
	go func() { defer close(done); s.CheckinAll() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CheckinAll must not jitter (still running)")
	}
	// 三个账号都真的走了签到 + 余额回写（证明抖动没被误用、账号也没被漏掉）。
	for _, uid := range []string{"u1", "u2", "u3"} {
		if st, _ := p.Status(uid); st.Credits != 999 {
			t.Errorf("%s credits=%d want 999", uid, st.Credits)
		}
	}
}

// TestCreditRefreshWritesPoolAndHistory 积分刷新：回写池余额并落一条 kind=probe 历史。
func TestCreditRefreshWritesPoolAndHistory(t *testing.T) {
	srv, _ := newCheckinServer(t)
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	hf := filepath.Join(t.TempDir(), "history.jsonl")
	s := New(Config{Pool: p, Upstream: up, HistoryFile: hf})

	s.RunCreditRefreshNow()
	// 余额回写（stub 的 resourceRemain）。
	if st, _ := p.Status("u1"); st.Credits != 999 {
		t.Errorf("pool credits=%d want 999", st.Credits)
	}
	entries := readHistory(t, hf)
	if len(entries) != 1 || entries[0].Kind != history.Probe {
		t.Fatalf("history=%+v want one probe entry", entries)
	}
	if entries[0].Remain != 999 || entries[0].UID != "u1" {
		t.Errorf("entry=%+v want uid=u1 remain=999", entries[0])
	}
}

// TestCheckinRecordsHistory 签到成功落一条 kind=checkin 快照（日报算当日消耗的基线来源）。
func TestCheckinRecordsHistory(t *testing.T) {
	srv, _ := newCheckinServer(t)
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	hf := filepath.Join(t.TempDir(), "history.jsonl")
	s := New(Config{Pool: p, Upstream: up, HistoryFile: hf})

	if _, err := s.CheckinAll(); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	entries := readHistory(t, hf)
	if len(entries) != 1 || entries[0].Kind != history.Checkin {
		t.Fatalf("history=%+v want one checkin entry", entries)
	}
	// 当日基线应能由该记录推出（日报口径的核心契约）。
	date := time.Now().Format("2006-01-02")
	if remain, _, ok, _ := history.BaselineOf(entries, date, "u1"); !ok || remain != 999 {
		t.Errorf("baseline=%d ok=%v want 999 true", remain, ok)
	}
}

// TestHistoryDisabledNoFile HistoryFile 为空时不写文件（零改动现状）。
func TestHistoryDisabledNoFile(t *testing.T) {
	srv, _ := newCheckinServer(t)
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	// 不应 panic，也不应尝试写任何路径。
	s.RunCreditRefreshNow()
	if _, err := s.CheckinAll(); err != nil {
		t.Fatalf("checkin: %v", err)
	}
}

// TestRunCreditRefreshLoopStopsOnCancel ctx 取消时刷新循环立即退出（不漏 goroutine）。
func TestRunCreditRefreshLoopStopsOnCancel(t *testing.T) {
	srv, _ := newCheckinServer(t)
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	// 极短周期让循环至少跑一轮。
	s := New(Config{Pool: p, Upstream: up, CreditRefresh: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.runCreditRefreshLoop(ctx) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("credit refresh loop must exit on ctx cancel")
	}
}

// readHistory 读取 JSONL 快照文件（测试辅助）。
func readHistory(t *testing.T, fp string) []history.Entry {
	t.Helper()
	f, err := os.Open(fp)
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	defer f.Close()
	var out []history.Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e history.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse entry %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// newCheckinServer 启动可用的签到/余额 stub（resourceRemain=999），复用既有
// checkinStub 的 server() 装配，避免在本文件重复一份假上游。
func newCheckinServer(t *testing.T) (*httptest.Server, *checkinStub) {
	t.Helper()
	f := &checkinStub{
		checkinBody:    `{"code":0,"msg":"ok","data":{}}`,
		resourceRemain: 999,
	}
	return f.server(), f
}
