package server

import (
	"strings"
	"testing"
)

// TestResolveModel 覆盖 PLAN D6 前缀解析协议 + 别名层，全程大小写不敏感：
// 第 1 层取第一个 ":"，前段小写折叠后命中前缀表（cn/global/同义 g）才剥离，否则 (cn, 原串)；
// 第 2 层对 bare 归一（小写折叠 + 别名表，与 realm 无关，单跳幂等）。
func TestResolveModel(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"glm-5.2", "cn", "glm-5.2"},
		{"deepseek:v3", "cn", "deepseek:v3"}, // 冒号前段不在前缀表内 → 整串原样（不折叠）

		// 前缀同义：g: ≡ global:（通用，任何 global 模型都适用）
		{"g:glm-5.3", "global", "glm-5.3"},
		{"g:gpt-5.4", "global", "gpt-5.4"},

		// 别名层：deepseek-flash → deepseek-v4.1-flash（前缀解析之后生效、与 realm 无关）
		{"g:deepseek-flash", "global", "deepseek-v4.1-flash"},
		{"global:deepseek-flash", "global", "deepseek-v4.1-flash"},
		{"deepseek-flash", "cn", "deepseek-v4.1-flash"}, // 裸名 → CN 域的 v4.1
		{"cn:deepseek-flash", "cn", "deepseek-v4.1-flash"},

		// 幂等：别名目标自身不变（含带前缀形态）
		{"deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"},
		{"global:deepseek-v4.1-flash", "global", "deepseek-v4.1-flash"},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestResolveModelCaseInsensitive 大小写不敏感（前缀与模型名两层都折叠）：
// 上游对模型名严格大小写敏感（GLM-5.3 → 11102），客户端大写拼写必须归一到规范小写。
func TestResolveModelCaseInsensitive(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		// 前缀层折叠：大写/混合大小写前缀与规范形等价
		{"G:glm-5.3", "global", "glm-5.3"},
		{"Global:gpt-5.4", "global", "gpt-5.4"},
		{"GLOBAL:gpt-5", "global", "gpt-5"},
		{"CN:glm-5.2", "cn", "glm-5.2"},

		// 模型名层折叠：大写拼写归一到小写规范形
		{"GLM-5.3", "cn", "glm-5.3"},
		{"Glm-5.3", "cn", "glm-5.3"},
		{"DeepSeek-V4.1-Flash", "cn", "deepseek-v4.1-flash"},

		// 两层叠加：大写前缀 + 大写别名拼写 → 规范 realm + 规范别名目标
		{"G:Deepseek-Flash", "global", "deepseek-v4.1-flash"},
		{"GLOBAL:DEEPSEEK-FLASH", "global", "deepseek-v4.1-flash"},
		{"CN:DeepSeek-Flash", "cn", "deepseek-v4.1-flash"},

		// 未知前缀仍原样（不因折叠而被"解释"成合法前缀）
		{"Deepseek:V3", "cn", "Deepseek:V3"},
		{"X:GLM-5.3", "cn", "X:GLM-5.3"},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestResolveModelEdge 边界：空串、仅冒号、空前缀、空裸名同义形态。
func TestResolveModelEdge(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"", "cn", ""},
		{":", "cn", ":"},
		{":model", "cn", ":model"}, // 空前缀不匹配，不剥离
		{"global:", "global", ""},  // 前缀合法 + 空裸名仍剥离
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"cn:", "cn", ""},
		{"g:", "global", ""}, // 同义前缀与 global: 同构（剥前缀、空裸名）
		{"g:deepseek-v4.1-flash", "global", "deepseek-v4.1-flash"},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestResolveModelUnknownPrefixVerbatim 未知前缀"整串原样"是刻意保留的语义：
// 不折叠、不剥前缀——上游 ID 不含 ":"，带冒号的串必然 11102，原样回显便于按客户端原文排错。
func TestResolveModelUnknownPrefixVerbatim(t *testing.T) {
	for _, in := range []string{"deepseek:v3", "Deepseek:V3", "ds:glm-4", "X:Y:Z", ":"} {
		realm, bare := resolveModel(in)
		if realm != "cn" || bare != in {
			t.Errorf("未知前缀 resolveModel(%q)=(%q,%q) want (cn,%q)", in, realm, bare, in)
		}
	}
}

// TestModelAliasesInvariants 别名表不变量（新增别名时自动生效）：
// 键值均小写、value 非空、value/key 不含 ":"、value 不是 key（禁链式）、表非空。
func TestModelAliasesInvariants(t *testing.T) {
	if len(modelAliases) == 0 {
		t.Fatal("modelAliases 为空：本测试假定至少有一条别名")
	}
	for key, val := range modelAliases {
		if key == "" {
			// 空键会把空裸名（"g:" / "cn:" / "global:" 剥前缀后）改写成某个模型，
			// 违背 resolveModel「空裸名沿用旧协议」的声明。
			t.Error("别名 key 不得为空串（会劫持空裸名）")
		}
		if val == "" {
			t.Errorf("别名 %q 的 value 为空", key)
		}
		if strings.Contains(val, ":") {
			t.Errorf("别名 %q → %q：value 不得含 ':'（必须是真实上游 ID）", key, val)
		}
		if strings.Contains(key, ":") {
			t.Errorf("别名 key %q 不得含 ':'（会与未知前缀形态的整串撞车）", key)
		}
		// 键值必须已是小写：查找按小写折叠进行，大写键永远查不中；value 会原样发给
		// 大小写敏感的上游，也必须是小写规范形。
		if key != strings.ToLower(key) {
			t.Errorf("别名 key %q 不是小写规范形（查找按小写折叠，大写键永远查不中）", key)
		}
		if val != strings.ToLower(val) {
			t.Errorf("别名 %q → %q：value 不是小写规范形（会原样发给大小写敏感的上游）", key, val)
		}
		if _, chained := modelAliases[val]; chained {
			t.Errorf("别名 %q → %q 构成链式（%q 也是 key）：只允许单跳", key, val, val)
		}
	}
}

// TestResolveModelAliasIdempotent 别名幂等 + realm 无关性（含大小写变体）：
//   - 别名目标自身再解析仍是自身（不自我映射）；
//   - "g:<key>" 与 "global:<key>" 解析出的 bare 恒等（别名层在前缀层之后、与 realm 无关）；
//   - 无论键的何种大小写拼写，都归一到同一个规范目标。
func TestResolveModelAliasIdempotent(t *testing.T) {
	for key, val := range modelAliases {
		if _, bare := resolveModel(val); bare != val {
			t.Errorf("别名目标 %q 再解析得 %q，want %q（幂等性破坏）", val, bare, val)
		}
		_, bareG := resolveModel("g:" + key)
		_, bareGlobal := resolveModel("global:" + key)
		if bareG != bareGlobal || bareG != val {
			t.Errorf("别名 %q：g:→%q global:→%q want 均为 %q（realm 无关性破坏）", key, bareG, bareGlobal, val)
		}
		titleKey := strings.ToUpper(key[:1]) + key[1:] // 首字母大写形态（strings.Title 已废弃）
		for _, variant := range []string{strings.ToUpper(key), titleKey, "G:" + strings.ToUpper(key)} {
			if _, bare := resolveModel(variant); bare != val {
				t.Errorf("大小写变体 %q 解析得 %q want %q", variant, bare, val)
			}
		}
	}
}

// TestResolveModelStripsClientMarkers 第 0 层：剥离客户端/代理层标记后再走前缀+别名解析。
//
// 回归背景（2026-10-02 用户实际报障）：CyberSlots 的 dsh(sidechat) 会话模型为
// "wb/wb/g:deepseek-flash"（provider 标记叠了两层），ai-proxy 剥掉一层转发，网关收到
// "wb/g:deepseek-flash"。不剥标记时：冒号前段 "wb/g" 不在前缀表 → realm 落 cn、bare
// 整串原样出站 → 上游 11102「model [wb/g:deepseek-flash] service info not found」→
// 5 个 CN 号逐个吃 6h 负缓存、客户端连吃 429/503。
func TestResolveModelStripsClientMarkers(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		// provider 归属前缀（wb/ · wb:，可叠层，大小写不敏感）
		{"wb/g:deepseek-flash", "global", "deepseek-v4.1-flash"},
		{"wb/wb/g:deepseek-flash", "global", "deepseek-v4.1-flash"},
		{"wb:g:deepseek-flash", "global", "deepseek-v4.1-flash"},
		{"WB/G:DeepSeek-Flash", "global", "deepseek-v4.1-flash"},
		{"wb/global:deepseek-v4.1-flash", "global", "deepseek-v4.1-flash"},
		{"wb/cn:glm-5.3", "cn", "glm-5.3"},
		{"wb/deepseek-flash", "cn", "deepseek-v4.1-flash"}, // 剥标记后裸名仍落 cn（协议不变）

		// 尾部上下文档位标记（数字 + K/M）
		{"global:deepseek-v4.1-flash[1M]", "global", "deepseek-v4.1-flash"},
		{"wb:global:deepseek-v4.1-flash[1M]", "global", "deepseek-v4.1-flash"},
		{"deepseek-v4.1-flash[300K]", "cn", "deepseek-v4.1-flash"},
		{"g:glm-5.3[64k]", "global", "glm-5.3"},

		// 反例：不构成标记的形态必须原样保留（宁可出站后按 11102 排错，也不猜）
		{"wbx/g:deepseek-flash", "cn", "wbx/g:deepseek-flash"}, // "wb" 后必须紧跟分隔符
		{"wbg:deepseek-flash", "cn", "wbg:deepseek-flash"},     // 冒号前段 "wbg" 不是 wb
		{"g:model[test]", "global", "model[test]"},             // 括号内非数字+KM → 不剥
		{"g:model[1x]", "global", "model[1x]"},
		{"wb/", "cn", "wb/"}, // 剥空则保留原串
		{"wb:", "cn", "wb:"},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}
