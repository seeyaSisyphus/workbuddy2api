// alias_test.go 验证 "reasoning" 别名字段：流式与非流式两条路径、
// 开启/关闭、非空/空思考四象限。别名字段是纯粹的**追加**语义：
// 必须与 reasoning_content 同值同生命周期，且绝不能改写 content/tool_calls。
package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// reasonFrame 构造一帧带 reasoning_content 的上游 SSE 数据行。
func reasonFrame(reasoning string) string {
	return `data: {"id":"x1","choices":[{"index":0,"delta":{"reasoning_content":"` + reasoning + `","content":"hi"}}]}` + "\n\n"
}

// TestStreamAliasReasoningField 流式：开启时非空思考帧同时带 reasoning 与
// reasoning_content（等值），关闭时只带 reasoning_content。
func TestStreamAliasReasoningField(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(reasonFrame("think")), true); err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	// 取第一条 data: 帧解析。
	line := strings.SplitN(rec.Body.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
		t.Fatalf("parse frame: %v (body=%q)", err, rec.Body.String())
	}
	delta := frame["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["reasoning"] != "think" {
		t.Errorf("alias reasoning=%v want \"think\"", delta["reasoning"])
	}
	if delta["reasoning_content"] != "think" {
		t.Errorf("reasoning_content=%v want \"think\"", delta["reasoning_content"])
	}

	// 关闭：reasoning_content 保留，reasoning 不出现。
	rec2 := httptest.NewRecorder()
	if err := Stream(rec2, strings.NewReader(reasonFrame("think")), false); err != nil {
		t.Fatal(err)
	}
	line2 := strings.SplitN(rec2.Body.String(), "\n", 2)[0]
	var frame2 map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line2, "data: ")), &frame2); err != nil {
		t.Fatal(err)
	}
	delta2 := frame2["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if _, ok := delta2["reasoning"]; ok {
		t.Errorf("alias must be absent when disabled: %v", delta2)
	}
	if delta2["reasoning_content"] != "think" {
		t.Errorf("reasoning_content must survive: %v", delta2)
	}
}

// TestStreamAliasSkipsEmptyReasoning 空思考不补别名（避免给客户端塞空字段）。
func TestStreamAliasSkipsEmptyReasoning(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(reasonFrame("")), true); err != nil {
		t.Fatal(err)
	}
	line := strings.SplitN(rec.Body.String(), "\n", 2)[0]
	var frame map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
		t.Fatal(err)
	}
	delta := frame["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if _, ok := delta["reasoning"]; ok {
		t.Errorf("empty reasoning must not be aliased: %v", delta)
	}
}

// TestAggregateAliasReasoningField 非流式：开启时 message 上补等值 reasoning，
// 关闭时不补；content 不受影响。
func TestAggregateAliasReasoningField(t *testing.T) {
	raw := strings.Join([]string{
		reasonFrame("think"),
		`data: {"id":"x1","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}, "")

	resp, err := Aggregate(strings.NewReader(raw), true)
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning"] != "think" {
		t.Errorf("alias reasoning=%v want \"think\"", msg["reasoning"])
	}
	if msg["reasoning_content"] != "think" {
		t.Errorf("reasoning_content=%v want \"think\"", msg["reasoning_content"])
	}
	if msg["content"] != "hi!" {
		t.Errorf("content=%v want \"hi!\"", msg["content"])
	}

	resp2, err := Aggregate(strings.NewReader(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	msg2 := resp2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg2["reasoning"]; ok {
		t.Errorf("alias must be absent when disabled: %v", msg2)
	}
	if msg2["reasoning_content"] != "think" {
		t.Errorf("reasoning_content must survive: %v", msg2)
	}
}

// TestAggregateAliasSkipsEmptyReasoning 无思考的响应不出现 reasoning 键。
func TestAggregateAliasSkipsEmptyReasoning(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(raw), true)
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["reasoning"]; ok {
		t.Errorf("no reasoning expected: %v", msg)
	}
}
