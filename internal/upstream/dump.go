package upstream

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/logfmt"
)

// badParamsDumpDir 上游拒绝请求参数（ErrBadParams）时的出站 body 落盘目录。
//
// 用途：定位 11101/11133 的**触发条件**。该错误间歇发生（2026-10-01 全天 7 次，
// 同一账号前后相邻请求均 200），客户端日志只记到 session 层、没有请求体，
// 网关侧的 WARN 日志也只有上游**响应**。没有出站 body 就无法回答"到底哪个参数
// 被拒"——上游返回的 extError.param 字段实测为空串，不给提示。
//
// 只在 ErrBadParams 路径写盘：该错误罕见（日均个位数），不做常驻开销，也无需
// 重启带环境变量（原 WB2A_DUMP_REQ 钩子要求 env 且只落大请求，不适合本场景）。
// 落盘内容含用户对话原文，仅供本机离线排查；保留最近 badParamsDumpKeep 份。
var badParamsDumpDir = func() string {
	if d := os.Getenv("WB2A_BADPARAMS_DUMP_DIR"); d != "" {
		return d
	}
	return filepath.Join("data", "badparams")
}()

// badParamsDumpKeep 保留最近 N 份转储（超出按文件名时间序删最旧）。
const badParamsDumpKeep = 20

// dumpBadParams 把被上游拒绝的**出站**请求体落盘，供离线定位触发参数。
//
// prepared 是经 prepareBody（effort 归一 + prompt_cache_key 注入）与
// ensureConsoleSystem（console 域 system 兜底）之后的字节，即真正发给上游的内容——
// 排查"哪个参数被拒"必须用这一份，而不是客户端原始 body（两者差异正是嫌疑之一）。
// 落盘失败只记日志，绝不影响请求链路。
func dumpBadParams(prepared, respBody []byte, uid, path string, status int) {
	if err := os.MkdirAll(badParamsDumpDir, 0o755); err != nil {
		log.Printf("WARN: [upstream] badparams dump mkdir: %v", err)
		return
	}
	stamp := time.Now().Format("20060102-150405.000")
	name := fmt.Sprintf("%s-%s-%d.json", stamp, logfmt.UID8(uid), status)
	fp := filepath.Join(badParamsDumpDir, name)

	// 出站 body 是 JSON（prepareBody 解析失败时会原样返回，故做一次校验兜底）：
	// 合法则嵌为 JSON 对象便于阅读，否则转义成字符串，两种情况都不丢信息。
	var req any
	if json.Valid(prepared) {
		req = json.RawMessage(prepared)
	} else {
		req = string(prepared)
	}
	doc := map[string]any{
		"ts":          time.Now().Format(time.RFC3339Nano),
		"uid":         uid,
		"path":        path,
		"status":      status,
		"request":     req,
		"responseRaw": string(respBody),
	}
	buf, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		log.Printf("WARN: [upstream] badparams dump marshal: %v", err)
		return
	}
	if err := os.WriteFile(fp, buf, 0o600); err != nil {
		log.Printf("WARN: [upstream] badparams dump write: %v", err)
		return
	}
	log.Printf("WARN: [upstream] badparams dump written: %s (uid=%s bytes=%d)",
		fp, logfmt.UID8(uid), len(prepared))
	pruneBadParamsDumps()
}

// pruneBadParamsDumps 只保留最近 badParamsDumpKeep 份转储（文件名前缀为可排序时间戳）。
func pruneBadParamsDumps() {
	ents, err := os.ReadDir(badParamsDumpDir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= badParamsDumpKeep {
		return
	}
	sort.Strings(names) // 时间戳前缀 → 字典序即时间序
	for _, n := range names[:len(names)-badParamsDumpKeep] {
		_ = os.Remove(filepath.Join(badParamsDumpDir, n))
	}
}
