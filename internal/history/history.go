// Package history 每日积分基线/消耗统计的持久化记录（append-only JSONL）。
// 每条记录是一份某账号某时刻的余额快照；签到成功写入 kind=checkin（含当日奖励），
// 其它余额刷新（signin 工具、credit 查询）写入 kind=probe。
// 当日基线 = 当天 kind=checkin 的最后一条；当天未签到则回退到 date 之前最后一条（近似）。
// 今日消耗 = 当日基线 − 实时余额。
package history

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Kind 记录类型。
type Kind string

const (
	Checkin Kind = "checkin" // 签到后权威余额（含当日奖励）
	Probe   Kind = "probe"   // 其它余额刷新（signin/credit）
)

// Entry 一条积分快照。
type Entry struct {
	Date   string `json:"date"` // 本地日期 YYYY-MM-DD
	TS     int64  `json:"ts"`   // Unix 秒
	Kind   Kind   `json:"kind"`
	UID    string `json:"uid"`
	Remain int64  `json:"remain"`
}

// DefaultPath 从 state_file 派生 history 文件路径（同目录 history.jsonl）。
func DefaultPath(stateFile string) string {
	if stateFile == "" {
		stateFile = "data/state.json"
	}
	return filepath.Join(filepath.Dir(stateFile), "history.jsonl")
}

// Append 追加一条快照；fp 为空时静默跳过。O_APPEND 单行原子写，跨进程安全。
func Append(fp string, e Entry) error {
	if fp == "" {
		return nil
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(fp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open history: %w", err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("append history: %w", err)
	}
	return f.Close()
}

// Load 读取全部记录（按行顺序返回）；文件不存在返回空切片。
func Load(fp string) ([]Entry, error) {
	f, err := os.Open(fp)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil {
			continue // 容忍脏行
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// BaselineOf 求 uid 在 date 当天的基线：
// 返回 (remain, 命中记录, 是否找到, 是否近似)。
// 优先当天最后一条 kind=checkin（含当日奖励）；当天未签到则回退 date 之前最后一条任意记录（approx=true）。
func BaselineOf(entries []Entry, date, uid string) (remain int64, base Entry, ok bool, approx bool) {
	var prev *Entry
	var dayCheckin *Entry
	for i := range entries {
		e := &entries[i]
		if e.UID != uid {
			continue
		}
		switch {
		case e.Date < date:
			prev = e
		case e.Date == date && e.Kind == Checkin:
			dayCheckin = e // 循环推进，落点为当天最后一条 checkin
		}
	}
	if dayCheckin != nil {
		return dayCheckin.Remain, *dayCheckin, true, false
	}
	if prev != nil {
		return prev.Remain, *prev, true, true
	}
	return 0, Entry{}, false, false
}
