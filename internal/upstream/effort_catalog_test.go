package upstream

import (
	"reflect"
	"testing"
)

// TestEffortListingRemoteWins 远端已解析档位为权威：无视静态表，直接用 remoteEfforts。
func TestEffortListingRemoteWins(t *testing.T) {
	efforts, def := EffortListing("cn", "glm-5.2",
		[]string{"low", "high"}, "low")
	if !reflect.DeepEqual(efforts, []string{"low", "high"}) {
		t.Errorf("efforts=%v want [low high] (remote authoritative)", efforts)
	}
	if def != "low" {
		t.Errorf("defaultEffort=%q want low (remote default, ∈ efforts)", def)
	}
}

// TestEffortListingStaticFallback 远端无档位 → 落到 CN 静态兜底表（照抄参考仓库档位）。
func TestEffortListingStaticFallback(t *testing.T) {
	cases := []struct {
		realm, model string
		wantEfforts  []string
		wantDefault  string
	}{
		{"cn", "deepseek-v4.1-flash", []string{"low", "high", "max"}, "high"},
		{"cn", "deepseek-v4-pro", []string{"low", "high", "xhigh"}, "high"},
		{"cn", "glm-5.3", []string{"low", "high", "max"}, "high"},
		{"cn", "glm-5.2", []string{"high", "xhigh"}, "high"},
		{"cn", "hy4-preview", []string{"high"}, "high"},
		{"", "deepseek-v4.1-flash", []string{"low", "high", "max"}, "high"}, // 空 realm 视作 cn
	}
	for _, c := range cases {
		efforts, def := EffortListing(c.realm, c.model, nil, "")
		if !reflect.DeepEqual(efforts, c.wantEfforts) {
			t.Errorf("%s/%s: efforts=%v want %v", c.realm, c.model, efforts, c.wantEfforts)
		}
		if def != c.wantDefault {
			t.Errorf("%s/%s: defaultEffort=%q want %q", c.realm, c.model, def, c.wantDefault)
		}
	}
}

// TestEffortListingGlobalDeepSeekEfforts global DeepSeek 档位来自上游直连实测：
// minimal 到 max 均可执行，off 不在能力表内；reasoning.effort=high 只是默认档。
func TestEffortListingGlobalDeepSeekEfforts(t *testing.T) {
	want := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	for _, model := range []string{"deepseek-v4.1-flash", "deepseek-v4.1-flash-sg"} {
		efforts, def := EffortListing("global", model, nil, "")
		if !reflect.DeepEqual(efforts, want) {
			t.Errorf("global %s efforts=%v want %v", model, efforts, want)
		}
		if def != "high" {
			t.Errorf("global %s defaultEffort=%q want high", model, def)
		}
	}
}

// TestEffortListingUnknownOmits 三级皆无 → efforts 返回 nil（调用方省略字段，非空数组）。
func TestEffortListingUnknownOmits(t *testing.T) {
	efforts, def := EffortListing("cn", "no-such-model", nil, "")
	if efforts != nil {
		t.Errorf("efforts=%v want nil (unknown model → omit field)", efforts)
	}
	if def != "" {
		t.Errorf("defaultEffort=%q want empty", def)
	}
	// global 域未知模型同理。
	efforts, _ = EffortListing("global", "deep-model", nil, "")
	if efforts != nil {
		t.Errorf("global deep-model efforts=%v want nil (该模型无档位声明)", efforts)
	}
}

// TestEffortListingDefaultNotInEffortsOmitted defaultEffort 不在档位表时不得宣称
// （对齐参考仓库 resolveModel 的 `defaultEffort ∈ efforts` 防御）。
func TestEffortListingDefaultNotInEffortsOmitted(t *testing.T) {
	efforts, def := EffortListing("cn", "m", []string{"low", "high"}, "max")
	if !reflect.DeepEqual(efforts, []string{"low", "high"}) {
		t.Errorf("efforts=%v want [low high]", efforts)
	}
	if def != "" {
		t.Errorf("defaultEffort=%q want empty (max ∉ efforts)", def)
	}
}
