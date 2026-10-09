package main

import (
	"reflect"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// realmPool 构造含 cn/global 账号的池并确保 global 开关开启（缺省）。
func realmPool(t *testing.T) *pool.Pool {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	return p
}

// TestRealmAwareAvailableForModel 断言会话粘性的 realm 感知闭包：
// 带前缀的模型名按 realm 过滤可用账号，裸名走 cn；同义前缀 g: 与别名层同口径。
func TestRealmAwareAvailableForModel(t *testing.T) {
	p := realmPool(t)
	fn := realmAwareAvailableForModel(p)

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1"}},           // 裸名 → cn 集合
		{"cn:glm-5.2", []string{"cn1"}},        // 显式 cn 前缀 → cn 集合
		{"global:gpt-5.4", []string{"g1"}},     // global 前缀 → global 集合
		{"g:gpt-5.4", []string{"g1"}},          // 同义前缀 g: → global 集合
		{"deepseek-flash", []string{"cn1"}},    // 裸别名 → cn 集合
		{"cn:deepseek-flash", []string{"cn1"}}, // cn + 别名 → cn 集合
		{"g:deepseek-flash", []string{"g1"}},   // g: + 别名 → global 集合
		{"global:deepseek-flash", []string{"g1"}},
		// 大小写不敏感：前缀层与模型名层都折叠，结论与规范拼写一致
		{"G:gpt-5.4", []string{"g1"}},
		{"GLOBAL:gpt-5.4", []string{"g1"}},
		{"CN:glm-5.2", []string{"cn1"}},
		{"GLM-5.3", []string{"cn1"}},
		{"G:Deepseek-Flash", []string{"g1"}},
	}
	for _, c := range cases {
		if got := fn(c.model); !reflect.DeepEqual(got, c.want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
}

// TestRealmAwareAvailableForModelAliasReachesPoolKey 别名必须落到 pool 的模型键上
// （否则粘性可用集合与选号集合会错配）：把 g1 对**别名目标**做 6004 模型级冷却后，
// 用别名拼写请求该模型应不可用，而其他模型不受影响。
func TestRealmAwareAvailableForModelAliasReachesPoolKey(t *testing.T) {
	p := realmPool(t)
	fn := realmAwareAvailableForModel(p)

	p.CooldownSoftForModel("g1", time.Minute, time.Now().Add(time.Minute), "deepseek-v4.1-flash", "6004 model rate limit")

	if got := fn("g:deepseek-flash"); len(got) != 0 {
		t.Errorf("别名未落到 pool 键：AvailableForModel(g:deepseek-flash)=%v want empty（g1 对别名目标已 6004 冷却）", got)
	}
	if got := fn("g:other-model"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("AvailableForModel(g:other-model)=%v want [g1]（冷却只应作用于该模型）", got)
	}
	// 大小写变体拼写必须落到同一个 pool 键：否则大写拼写的失败避让永远避不开
	// （冷却键写 `deepseek-v4.1-flash`、请求键用 `G:DeepSeek-Flash` 就错配）。
	if got := fn("G:DeepSeek-Flash"); len(got) != 0 {
		t.Errorf("大小写变体未归一到 pool 键：AvailableForModel(G:DeepSeek-Flash)=%v want empty", got)
	}
}

// TestRealmAwareAvailableForModelGlobalDisabled 逃生门：全局开关显式关闭（config
// "enabled": false）时，即便 auth 写了 realm=global 也不路由 global——闭包对 global:
// 前缀返回空集（纯 CN 锁定语义与旧缺省等价）。
func TestRealmAwareAvailableForModelGlobalDisabled(t *testing.T) {
	auth.SetGlobalEnabled(false)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	fn := realmAwareAvailableForModel(p)

	// 开关关闭 → 该 global 账号 Realm()=="cn"（逃生门），对 global: / 同义 g: 前缀均不可见
	// （含大小写变体：前缀折叠后仍归 global，逃生门不能因大小写被绕过）。
	for _, m := range []string{"global:gpt-5.4", "g:gpt-5.4", "G:gpt-5.4", "GLOBAL:gpt-5.4"} {
		if got := fn(m); len(got) != 0 {
			t.Errorf("global disabled: AvailableForModel(%q)=%v want empty", m, got)
		}
	}
	// 裸名 → cn：该账号被当作 cn 可见（现状等价，纯 CN 锁定）。
	if got := fn("glm-5.2"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("global disabled: AvailableForModel(glm-5.2)=%v want [g1]", got)
	}
}

// TestRealmAwareAvailableForModelDefaultOnCNZeroRegression 开关缺省开启（生产语义）时
// 纯 CN 部署零回归：cn 账号（cn domain / 空 domain）对裸名与 cn: 前缀都可见，
// global: 前缀对纯 CN 池不可见（池里无 global 账号）。
func TestRealmAwareAvailableForModelDefaultOnCNZeroRegression(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: ""}) // 空 domain → cn（老 CN 凭证）
	fn := realmAwareAvailableForModel(p)

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1", "cn2"}},       // 裸名 → cn 集合（现状零回归）
		{"cn:glm-5.2", []string{"cn1", "cn2"}},    // cn 前缀 → cn 集合
		{"global:gpt-5.4", nil},                   // global 前缀 → 纯 CN 池无可用（不对 spread）
	}
	for _, c := range cases {
		want := c.want
		if want == nil {
			want = []string{}
		}
		if got := fn(c.model); !reflect.DeepEqual(got, want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, want)
		}
	}
}