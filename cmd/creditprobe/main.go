// creditprobe — 一次性只读运维探针：逐账号拉取积分套餐明细（含到期时刻）。
//
// 与 cmd/credit 的差异：不聚合，输出**每个套餐**的剩余积分与 DeductionEndTime，
// 供人工核对"哪些积分即将作废"。请求头/请求体与 upstream.ResourceSummary 完全一致
// （同一 BillingHeaders + 同一过滤体），不写任何本地状态、不触碰账号池。
//
// 用法: go run ./cmd/creditprobe > out.json
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

type pkg struct {
	PackageName         string `json:"PackageName"`
	DeductionEndTime    int64  `json:"DeductionEndTime"`
	CycleEndTime        string `json:"CycleEndTime"`
	Status              int    `json:"Status"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

type outPkg struct {
	Name     string `json:"name"`
	Remain   int64  `json:"remain"`
	Size     int64  `json:"size"`
	ExpireMS int64  `json:"expire_ms"`
	Expire   string `json:"expire"`
	CycleEnd string `json:"cycle_end,omitempty"`
	Status   int    `json:"status"`
}

type outAcct struct {
	UID      string   `json:"uid"`
	Nick     string   `json:"nick"`
	Realm    string   `json:"realm"`
	OK       bool     `json:"ok"`
	Err      string   `json:"err,omitempty"`
	Remain   int64    `json:"remain"`
	Expiring int64    `json:"expiring_7d"`
	Pkgs     []outPkg `json:"packages,omitempty"`
}

func main() {
	authDir := "./auths"
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	up := upstream.New()
	up.GlobalEnabled = true

	files, _ := auth.LoadAuthFiles(authDir)
	out := make([]outAcct, 0, len(files))
	now := time.Now()
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		rec := outAcct{UID: a.UID, Nick: a.Nickname, Realm: a.Realm()}
		pkgs, err := fetch(up, a, now)
		if err != nil {
			rec.Err = err.Error()
			out = append(out, rec)
			continue
		}
		rec.OK = true
		for _, p := range pkgs {
			remain, size := pkgRemain(p)
			if remain <= 0 {
				continue
			}
			rec.Remain += remain
			exp := ""
			if p.DeductionEndTime > 0 {
				t := time.UnixMilli(p.DeductionEndTime).In(time.Local)
				exp = t.Format("2006-01-02")
				if !t.After(now.Add(7 * 24 * time.Hour)) {
					rec.Expiring += remain
				}
			}
			rec.Pkgs = append(rec.Pkgs, outPkg{
				Name: p.PackageName, Remain: remain, Size: size,
				ExpireMS: p.DeductionEndTime, Expire: exp,
				CycleEnd: p.CycleEndTime, Status: p.Status,
			})
		}
		out = append(out, rec)
		time.Sleep(250 * time.Millisecond)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// pkgRemain 复刻 upstream.packageRemainUsed 的单套餐取数口径（Cycle 优先）。
func pkgRemain(p pkg) (remain, size int64) {
	if p.CycleCapacitySize > 0 {
		remain, size = p.CycleCapacityRemain, p.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		return remain, size
	}
	return p.CapacityRemain, p.CapacitySize
}

// fetch 走与生产一致的 headers + 请求体，拉取原始套餐列表。
func fetch(c *upstream.Client, a *auth.Auth, now time.Time) ([]pkg, error) {
	base := "https://www.codebuddy.cn"
	path := "/v2/billing/meter/get-user-resource"
	if a.IsGlobal() {
		base = "https://www.workbuddy.ai"
		path = "/billing/meter/get-user-resource"
	}
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, trunc(raw))
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []pkg `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data.Response.Data.Accounts, nil
}

func trunc(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}
