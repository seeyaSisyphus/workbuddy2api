// alias_test.go 模型名别名层的 handler 端到端契约测试（resolve_model.go 的
// realmPrefixAliases / modelAliases）。
//
// 别名是**入站便利层**：解析出 (realm, bare) 后，出站请求体、选号、粘性可用集合、
// 11102/6004 冷却键、成本账本一律用 bare（规范上游 ID）。本文件把这条链路钉成契约。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// TestChatModelAliasGlobalPrefix `g:deepseek-flash`（同义前缀 + 别名两层）：
// 路由到 global 账号、出站模型为规范裸名、走 /console 路径、补兜底 system。
func TestChatModelAliasGlobalPrefix(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cf := newRealmFake(t)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"g:deepseek-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("g:deepseek-flash code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel, gotPath, gotMsgs := cf.authz, cf.model, cf.path, cf.msgs
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_gl" {
		t.Errorf("authz=%q want global g1 (g: 应路由 global)", gotAuthz)
	}
	if gotModel != "deepseek-v4.1-flash" {
		t.Errorf("outbound model=%q want deepseek-v4.1-flash（前缀 + 别名均应剥除）", gotModel)
	}
	if gotPath != "/console/chat/completions" {
		t.Errorf("path=%q want /console/chat/completions", gotPath)
	}
	if gotMsgs != 2 { // ensureConsoleSystem：user 前置补 system
		t.Errorf("messages=%d want 2 (global 域补兜底 system)", gotMsgs)
	}
}

// TestChatModelAliasBareStaysCN 裸别名 `deepseek-flash`：realm 归 CN、出站模型为
// 规范裸名、走 /v2 路径、不补 system。
func TestChatModelAliasBareStaysCN(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cf := newRealmFake(t)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("deepseek-flash code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel, gotPath, gotMsgs := cf.authz, cf.model, cf.path, cf.msgs
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_cn" {
		t.Errorf("authz=%q want CN cn1 (裸别名归 CN)", gotAuthz)
	}
	if gotModel != "deepseek-v4.1-flash" {
		t.Errorf("outbound model=%q want deepseek-v4.1-flash", gotModel)
	}
	if gotPath != "/v2/chat/completions" {
		t.Errorf("path=%q want /v2/chat/completions", gotPath)
	}
	if gotMsgs != 1 {
		t.Errorf("messages=%d want 1 (CN 不补 system)", gotMsgs)
	}
}

// TestChatModelAliasCanonicalPrefixEquivalent 同义前缀等价：global:deepseek-flash 与
// g:deepseek-flash 的出站三元组（authz/model/path）逐字段相同。
func TestChatModelAliasCanonicalPrefixEquivalent(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)

	run := func(model string) (string, string, string) {
		cf := newRealmFake(t)
		h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != 200 {
			t.Fatalf("%s code=%d body=%s", model, rec.Code, rec.Body)
		}
		cf.mu.Lock()
		defer cf.mu.Unlock()
		return cf.authz, cf.model, cf.path
	}

	a1, m1, p1 := run("global:deepseek-flash")
	a2, m2, p2 := run("g:deepseek-flash")
	if a1 != a2 || m1 != m2 || p1 != p2 {
		t.Errorf("同义前缀不等价：\n  global: → (%q,%q,%q)\n  g:      → (%q,%q,%q)", a1, m1, p1, a2, m2, p2)
	}
}

// TestChatModelCaseInsensitive 大小写变体端到端：上游对模型名严格大小写敏感
// （GLM-5.3 → 11102），网关必须在出站前归一到小写规范形。
// 前缀层（G: / GLOBAL:）与模型名层（GLM-5.3 / DeepSeek-Flash）叠加大写也一并归一。
func TestChatModelCaseInsensitive(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cases := []struct {
		model    string
		wantAuth string
		wantMod  string
		wantPath string
	}{
		{"GLM-5.3", "Bearer at_cn", "glm-5.3", "/v2/chat/completions"},
		{"G:GLM-5.3", "Bearer at_gl", "glm-5.3", "/console/chat/completions"},
		{"GLOBAL:GPT-5.4", "Bearer at_gl", "gpt-5.4", "/console/chat/completions"},
		{"CN:Glm-5.2", "Bearer at_cn", "glm-5.2", "/v2/chat/completions"},
		{"G:DeepSeek-Flash", "Bearer at_gl", "deepseek-v4.1-flash", "/console/chat/completions"},
	}
	for _, c := range cases {
		cf := newRealmFake(t)
		p := testPoolWith(
			&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
			&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
		)
		h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+c.model+`","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != 200 {
			t.Fatalf("%s code=%d body=%s", c.model, rec.Code, rec.Body)
		}
		cf.mu.Lock()
		gotAuthz, gotModel, gotPath := cf.authz, cf.model, cf.path
		cf.mu.Unlock()
		if gotAuthz != c.wantAuth {
			t.Errorf("%s authz=%q want %q", c.model, gotAuthz, c.wantAuth)
		}
		if gotModel != c.wantMod {
			t.Errorf("%s outbound model=%q want %q（大小写必须归一）", c.model, gotModel, c.wantMod)
		}
		if gotPath != c.wantPath {
			t.Errorf("%s path=%q want %q", c.model, gotPath, c.wantPath)
		}
	}
}

// TestChatCaseVariantLedgerKeyUnified 大小写折叠把"同模型两个键"合并成一个：
// GLM-5.3 与 glm-5.3 折叠前在 pool.modelCost（成本账本）与 pool.modelCooldowns
// （11102 避让）里是两条独立记录——大写拼写的失败避让永远等不到成功清命，
// 成本观测也被拆成两份 EMA。折叠后必须落在同一键上。
func TestChatCaseVariantLedgerKeyUnified(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseWithCredit, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"GLM-5.3","messages":[],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if per1k, ok := p.ModelCost("u1", "glm-5.3"); !ok {
		t.Error("账本应落在规范小写键 glm-5.3 上")
	} else if per1k <= 0 {
		t.Errorf("账本 glm-5.3 per1k=%v want >0", per1k)
	}
	if per1k, ok := p.ModelCost("u1", "GLM-5.3"); ok {
		t.Errorf("大写拼写不得单独成键：u1/GLM-5.3 per1k=%v（账本分裂）", per1k)
	}
}

// TestChatAliasLedgerKeyIsBareModel 成本账本用规范裸名：客户端发别名拼写，
// 账本落在别名目标上，客户端原串绝不进账本。
func TestChatAliasLedgerKeyIsBareModel(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseWithCredit, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-flash","messages":[],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if _, ok := p.ModelCost("u1", "deepseek-v4.1-flash"); !ok {
		t.Error("账本应落在别名目标 deepseek-v4.1-flash 上")
	}
	if per1k, ok := p.ModelCost("u1", "deepseek-flash"); ok {
		t.Errorf("客户端原串不得进账本：u1/deepseek-flash per1k=%v", per1k)
	}
}

// TestChatAliasCooldownKeyIsBareModel 11102 负缓存键用规范裸名：客户端发别名拼写
// 撞 11102 后，限额台账显示别名目标，客户端原串不出现。
func TestChatAliasCooldownKeyIsBareModel(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 404, `{"code":11102,"msg":"model [deepseek-v4.1-flash] service info not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-flash","messages":[]}`)))
	if rec.Code == 200 {
		t.Fatalf("11102 应失败：code=%d", rec.Code)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 不在池中")
	}
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("rate_limited_models=%+v want 1 行", st.RateLimitedModels)
	}
	if got := st.RateLimitedModels[0].Model; got != "deepseek-v4.1-flash" {
		t.Errorf("限额台账模型=%q want deepseek-v4.1-flash（客户端原串不得进冷却键）", got)
	}
}

// TestChatCaseVariantCooldownKeyUnified 11102 避让键同样归一：大写拼写撞 11102 后，
// 避让必须写在小写规范键上——否则成功请求的 BlockModelClear("glm-5.3") 清不到
// "GLM-5.3" 条目，该号会一直被错误地避开这个模型。
func TestChatCaseVariantCooldownKeyUnified(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 404, `{"code":11102,"msg":"model [glm-5.3] service info not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"GLM-5.3","messages":[]}`)))
	if rec.Code == 200 {
		t.Fatalf("11102 应失败：code=%d", rec.Code)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 不在池中")
	}
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("rate_limited_models=%+v want 1 行", st.RateLimitedModels)
	}
	if got := st.RateLimitedModels[0].Model; got != "glm-5.3" {
		t.Errorf("11102 避让键=%q want glm-5.3（大写拼写须归一到同一键）", got)
	}
}

// TestChatAliasResponseModelEchoesUpstream 契约：别名只作用于**入站解析**，
// 响应体的 model 字段回显上游真实 ID（不做反查回客户端拼写）。
// 若日后有人把这里"修"成回显别名，本用例会红——请先确认产品意图再改。
func TestChatAliasResponseModelEchoesUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true // sseOK 的帧里 model 为 "glm-5.2"
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-flash","messages":[],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 流式透传：上游帧原样下发，model 字段是上游值（非客户端别名）。
	if !strings.Contains(rec.Body.String(), `"model":"glm-5.2"`) {
		t.Errorf("响应应原样透传上游 model 字段（别名只作用于入站解析）：\n%s", rec.Body.String())
	}
}

// TestChatAliasStickyRealmIndependent 粘性跨别名一致性：
//   - 同一会话连发裸别名（cn 域）→ 绑定稳定在同一 CN 号；
//   - 改发 g: 同义前缀（global 域）→ realm 不符 → 解绑并重分配到 global 号
//     （跨域切换本就应该换号，见 handler.go 的 acct.Realm()!=realm 校验）。
func TestChatAliasStickyRealmIndependent(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"cn1", "g1"} },
		AvailableForModel: func(model string) []string {
			realm, _ := ResolveModel(model)
			if realm == "global" {
				return []string{"g1"}
			}
			return []string{"cn1"}
		},
	})
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, GlobalEnabled: true})

	body := func(model string) *strings.Reader {
		raw, _ := json.Marshal(map[string]any{
			"model":    model,
			"messages": []any{},
			"metadata": map[string]any{"conversation_id": "conv-alias"},
		})
		return strings.NewReader(string(raw))
	}
	do := func(model string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", body(model)))
		if rec.Code != 200 {
			t.Fatalf("%s code=%d body=%s", model, rec.Code, rec.Body)
		}
	}

	// 两次裸别名 → 都落在 CN 号，绑定稳定。
	do("deepseek-flash")
	do("deepseek-flash")
	if uid, ok := st.lastUID("conv-alias"); !ok || uid != "cn1" {
		t.Fatalf("裸别名两次后绑定=%q ok=%v want cn1（同一会话应稳定）", uid, ok)
	}

	// 换 g: 同义前缀（global 域）→ realm 不符 → 解绑重分配，最终成功号 g1 成为新绑定。
	do("g:deepseek-flash")
	if uid, ok := st.lastUID("conv-alias"); !ok || uid != "g1" {
		t.Fatalf("g: 别名后绑定=%q ok=%v want g1（跨域应解绑重分配）", uid, ok)
	}
}
