package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestRetryAfterSecondsClamp Retry-After 换算：向上取整、钳 [1,60] 秒。
func TestRetryAfterSecondsClamp(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "1"},
		{300 * time.Millisecond, "1"},
		{1500 * time.Millisecond, "2"},
		{59 * time.Second, "59"},
		{90 * time.Second, "60"},
		{2 * time.Hour, "60"},
	}
	for _, c := range cases {
		if got := retryAfterSeconds(c.d); got != c.want {
			t.Errorf("retryAfterSeconds(%v)=%q want %q", c.d, got, c.want)
		}
	}
}

// TestChatGateLifecycle 故障门：阈值前不开、达阈值开、noteOK 复位。
func TestChatGateLifecycle(t *testing.T) {
	var g chatGate
	now := time.Now()
	for i := 1; i < gateFailThreshold; i++ {
		g.noteFail("m", now, 0)
		if _, open := g.open("m", now); open {
			t.Fatalf("gate opened after %d fails (threshold %d)", i, gateFailThreshold)
		}
	}
	g.noteFail("m", now, 0)
	wait, open := g.open("m", now)
	if !open || wait <= 0 {
		t.Fatalf("gate must open at threshold, open=%v wait=%v", open, wait)
	}
	// 其它模型的键不受影响（按模型隔离）。
	if _, open := g.open("other", now); open {
		t.Fatal("gate must be isolated per key")
	}
	g.noteOK("m")
	if _, open := g.open("m", now); open {
		t.Fatal("noteOK must close the gate")
	}
}

// TestChatGateHintClamped 上游给的恢复提示更晚时对齐提示，但封顶 gateBackoffMax。
func TestChatGateHintClamped(t *testing.T) {
	var g chatGate
	now := time.Now()
	for i := 0; i < gateFailThreshold; i++ {
		g.noteFail("m", now, time.Hour)
	}
	wait, open := g.open("m", now)
	if !open || wait != gateBackoffMax {
		t.Fatalf("hint clamp → want open %v, got (%v,%v)", gateBackoffMax, wait, open)
	}
}

// TestChatNoAccountCarriesRetryAfter 全池模型冷却 → 503 + Retry-After（1h 冷却钳到 60s）。
func TestChatNoAccountCarriesRetryAfter(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "glm-5.2", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q want 60（1h 冷却被钳到上限）", got)
	}
}

// TestChatFailGateOpensAfterConsecutiveFailures 连续失败达阈值 → 后续请求 429 + Retry-After
// （不再空转）；noteOK（等效一次成功）后回到 503 语义。
func TestChatFailGateOpensAfterConsecutiveFailures(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "glm-5.2", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		return rec
	}
	for i := 0; i < gateFailThreshold; i++ {
		if rec := post(); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("failure %d: code=%d want 503", i+1, rec.Code)
		}
	}
	rec := post()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d consecutive failures: code=%d want 429 (gate open)", gateFailThreshold, rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("gate response must carry Retry-After")
	}
	h.gate.noteOK("cn|glm-5.2")
	if rec := post(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("after reset: code=%d want 503 (gate closed)", rec.Code)
	}
}

// TestChatFailGateIgnoresInflightPressure 并发名额占满导致的 503 **不得**升级闸门。
//
// 场景：账号健康但把在途名额占满 → pick 无候选 → 503。此刻池子并非"没有可服务账号"，
// 只是名额一时占满（下一个请求完成即释放），属瞬时不可用。旧实现把它计入连败，
// 5 次瞬时拒绝就会开门封锁 2~30s，把 503 放大成 429（2026-09-29 下午实测：
// 30 并发下 5 次 503 被放大成 20 次失败）。对照 TestChatFailGateOpensAfterConsecutiveFailures
// （账号真在冷却 → 照常开门）与 TestChatNoAccountCarriesRetryAfter（冷却提示真实时长）。
func TestChatFailGateIgnoresInflightPressure(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") { // 占满唯一名额：账号健康但 pick 无候选
		t.Fatal("setup: acquire the only in-flight slot")
	}
	defer p.Release("u1")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		return rec
	}
	// 发到阈值的 4 倍：若闸门误升级，中途必然出现 429。
	for i := 0; i < gateFailThreshold*4; i++ {
		rec := post()
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("request %d: code=%d want 503（瞬时占满不应升级为 429）", i+1, rec.Code)
		}
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Fatalf("request %d: Retry-After=%q want \"1\"（存在健康号 → 只提示最短退避）", i+1, got)
		}
	}
	if _, open := h.gate.open("cn|glm-5.2", time.Now()); open {
		t.Fatal("瞬时并发占满不得开门")
	}
}

// TestChatFailGateOpensOnPersistentUpstreamError 上游**持续**报错（已选到号）必须升级闸门。
//
// 为什么不能只看"池子有没有健康号"：11133（model_param_invalid，上游拒绝请求参数）这类
// 失败**不冷却账号**，池子始终"有健康号"，于是整段风暴被漏计、闸门永不打开，客户端无人
// 阻挡地重试——2026-09-29 15:48~15:59 实测 30 次失败全被漏计，每次重试白烧 3 次上游
// 轮换（共 90 次无谓调用，正是最该避免的风控暴露）。
// 判据因此改为按「是否选到号」区分：st.uid != "" = 持久性失败 → 升级。
// 对照 TestChatFailGateIgnoresInflightPressure（同样"有健康号"，但一个号都没选到 → 不升级）。
//
// 2026-10-01 更新：11133 现在回 **400**（参数错是客户端侧，不再谎报 503 no_healthy_account），
// 但**仍计入闸门**——实测客户端会对 400 反复重试（同一 requestId 出现两次），不下闸门就会
// 演变成上游风暴。故本测试断言：前 5 次 400，第 6 次 429。
func TestChatFailGateOpensOnPersistentUpstreamError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":11133,"msg":"Invalid request parameters",`+
			`"extError":{"code":"model_param_invalid"}}`)
	}))
	defer ts.Close()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: ts.Client(), ChatBaseCN: ts.URL, ChatBaseGlobal: ts.URL}
	h := NewHandler(Config{Pool: p, Upstream: up})

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		return rec
	}
	// 上游 400 参数错 → 网关 400（客户端侧语义）；账号不冷却，池子始终有健康号
	// （旧「只看有没有健康号」判据下闸门永不开）。
	for i := 0; i < gateFailThreshold; i++ {
		if rec := post(); rec.Code != http.StatusBadRequest {
			t.Fatalf("failure %d: code=%d want 400 (invalid_request_error)", i+1, rec.Code)
		}
	}
	if rec := post(); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("上游持续报错 %d 次后应开门 429（旧判据「有健康号即不升级」会漏计），got %d",
			gateFailThreshold, rec.Code)
	}
}

// TestFormatWait 时长渲染：向上取整到展示单位，进位不出现 "60s"/"60m"/"2h60m"。
func TestFormatWait(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-time.Second, "0s"},
		{30 * time.Second, "30s"},
		{59*time.Second + 500*time.Millisecond, "60s"}, // 向上取整（不早于真实恢复时刻）
		{time.Minute, "1m"},
		{90 * time.Second, "2m"},
		{45 * time.Minute, "45m"},
		{59*time.Minute + 30*time.Second, "1h"}, // 进位，而非 "60m"
		{time.Hour, "1h"},
		{2*time.Hour + 20*time.Minute, "2h20m"},
		{2*time.Hour + 20*time.Minute + 30*time.Second, "2h21m"},
		{2*time.Hour + 59*time.Minute + 30*time.Second, "3h"}, // 进位，而非 "2h60m"
		{24 * time.Hour, "24h"},                               // 冷却上限即 24h（cappedResetUntilLocked），无需"天"单位
	}
	for _, c := range cases {
		if got := formatWait(c.d); got != c.want {
			t.Errorf("formatWait(%v)=%q want %q", c.d, got, c.want)
		}
	}
}

// TestChatNoAccountMessageNamesModel 全池模型冷却（ue==nil 的本地调度失败）→ 503 文案
// 必须点名**模型 + 域 + 恢复时长 + 可切模型**，而非通用的 "all accounts are temporarily
// unavailable"。
//
// 回归背景（2026-10-02 用户实际报障）：CN 五个号的 glm-5.3 全部被上游 6004 限额（同一时刻
// `g:glm-5.3` 立刻可用），客户端只看到通用 503，用户来问"我调用 cn glm，现在卡住了？"。
// 上游自己都会提示 "alternatively, you can switch to the other models"，网关没有理由不说。
func TestChatNoAccountMessageNamesModel(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	// 2h 后恢复的模型级冷却（resetAt 走精确对齐：until = resetAt + 90s 缓冲）。
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(2*time.Hour), "glm-5.3", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	out := rec.Body.String()
	for _, want := range []string{"glm-5.3", "on cn", "switch to another model", "~2h"} {
		if !strings.Contains(out, want) {
			t.Errorf("503 文案缺 %q：%s", want, out)
		}
	}
	if strings.Contains(out, "all accounts are temporarily unavailable") {
		t.Errorf("已知模型级恢复时刻时不该回通用文案：%s", out)
	}
}
