package main

import (
	"os"
	"testing"
)

// TestClearProxyEnv 直连优先：cleared 后所有代理环境变量都不再对
// http.ProxyFromEnvironment 可见（网关必须走家宽直连，否则台湾节点出口会触发上游 WAF 403）。
//
// 回归背景：2026-09-30 网关被以"绕过 start-silent.vbs"的方式拉起，继承了用户级
// HTTP_PROXY=127.0.0.1:7890 → 出口变台湾 → WAF 回归。修在程序内以摆脱对启动方式的依赖。
func TestClearProxyEnv(t *testing.T) {
	const fake = "http://127.0.0.1:7890"
	for _, k := range proxyEnvKeys {
		t.Setenv(k, fake) // t.Setenv 自动还原，不污染其它用例
	}
	clearProxyEnv()
	for _, k := range proxyEnvKeys {
		if got := os.Getenv(k); got != "" {
			t.Errorf("%s 未被清除，仍为 %q（网关会走代理 → WAF 风险）", k, got)
		}
	}
}

// TestClearProxyEnvIdempotent 未设置代理时是空操作（不 panic、不改动其它环境）。
func TestClearProxyEnvIdempotent(t *testing.T) {
	for _, k := range proxyEnvKeys {
		t.Setenv(k, "")
	}
	t.Setenv("WORKBUDDY2API_PROXYENV_SENTINEL", "keep")
	clearProxyEnv()
	if got := os.Getenv("WORKBUDDY2API_PROXYENV_SENTINEL"); got != "keep" {
		t.Errorf("空操作路径不应改动无关环境变量，got %q", got)
	}
}
