package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDumpBadParamsWritesBody ErrBadParams 落盘：出站 body 必须原样可还原（排查靠它）。
func TestDumpBadParamsWritesBody(t *testing.T) {
	dir := t.TempDir()
	old := badParamsDumpDir
	badParamsDumpDir = dir
	t.Cleanup(func() { badParamsDumpDir = old })

	req := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"reasoning_effort":"max"}`)
	resp := []byte(`{"code":11133,"msg":"Invalid request parameters"}`)
	dumpBadParams(req, resp, "5ee926af-1234", "/console/chat/completions", 400)

	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("want 1 dump file, got %v (err=%v)", len(ents), err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ents[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		UID     string          `json:"uid"`
		Path    string          `json:"path"`
		Status  int             `json:"status"`
		Request json.RawMessage `json:"request"`
		RespRaw string          `json:"responseRaw"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("dump not json: %v", err)
	}
	if doc.UID != "5ee926af-1234" || doc.Status != 400 || doc.Path != "/console/chat/completions" {
		t.Errorf("meta wrong: %+v", doc)
	}
	// 出站 body 必须语义可还原（这是排查的唯一依据）。MarshalIndent 会重排缩进，
	// 故按 JSON 语义比较而非逐字节：被查的是**参数值**，空白不影响上游判定。
	var got, want map[string]any
	if err := json.Unmarshal(doc.Request, &got); err != nil {
		t.Fatalf("dumped request not json: %v", err)
	}
	if err := json.Unmarshal(req, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("request keys=%v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("request[%q]=%v want %v", k, got[k], v)
		}
	}
}

// TestDumpBadParamsNonJSONBody 非 JSON body 不得丢信息（转义为字符串）。
func TestDumpBadParamsNonJSONBody(t *testing.T) {
	dir := t.TempDir()
	old := badParamsDumpDir
	badParamsDumpDir = dir
	t.Cleanup(func() { badParamsDumpDir = old })

	dumpBadParams([]byte("not-json-at-all"), []byte(`{}`), "u1", "/v2/chat/completions", 400)
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("want 1 file")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ents[0].Name()))
	var doc struct {
		Request string `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if doc.Request != "not-json-at-all" {
		t.Errorf("request=%q want raw string preserved", doc.Request)
	}
}

// TestDumpBadParamsKeepsLastN 转储目录只保留最近 N 份（防无限增长）。
func TestDumpBadParamsKeepsLastN(t *testing.T) {
	dir := t.TempDir()
	old := badParamsDumpDir
	badParamsDumpDir = dir
	t.Cleanup(func() { badParamsDumpDir = old })

	for i := 0; i < badParamsDumpKeep+5; i++ {
		dumpBadParams([]byte(`{}`), []byte(`{}`), "u1", "/v2", 400)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) > badParamsDumpKeep {
		t.Errorf("kept %d files, want <= %d", len(ents), badParamsDumpKeep)
	}
}
