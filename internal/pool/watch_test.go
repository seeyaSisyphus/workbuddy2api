package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// writeAuthFile 落一个最小可用凭证文件（字段与 internal/auth 读取格式一致）。
func writeAuthFile(t *testing.T, dir, uid, nick string) string {
	t.Helper()
	doc := map[string]any{
		"account": map[string]any{"uid": uid, "enterpriseId": "", "nickname": nick},
		"auth": map[string]any{
			"accessToken":  "tok-" + uid,
			"refreshToken": "ref-" + uid,
			"expiresAt":    time.Now().Add(24 * time.Hour).Unix(),
			"domain":       "www.workbuddy.ai",
			"realm":        "global",
		},
	}
	raw, _ := json.Marshal(doc)
	p := filepath.Join(dir, "workbuddy-"+uid+".json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDirFingerprintDetectsChange 指纹对「新增 / 修改 / 删除」三种变化都必须敏感。
func TestDirFingerprintDetectsChange(t *testing.T) {
	dir := t.TempDir()
	base, ok := dirFingerprint(dir)
	if !ok {
		t.Fatal("空目录应可读（ok=true）")
	}
	if base != "" {
		t.Errorf("空目录指纹应为空串，得到 %q", base)
	}

	// 新增
	writeAuthFile(t, dir, "u1", "一号")
	afterAdd, _ := dirFingerprint(dir)
	if afterAdd == base {
		t.Error("新增文件后指纹应变化")
	}

	// 内容修改（同名覆盖，模拟凭证刷新）：文件名不变，仅 mtime/size 变
	time.Sleep(10 * time.Millisecond)
	writeAuthFile(t, dir, "u1", "一号改")
	afterMod, _ := dirFingerprint(dir)
	if afterMod == afterAdd {
		t.Error("同名覆盖后指纹应变化（凭证刷新场景）")
	}

	// 非 json 文件不参与指纹
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fp, _ := dirFingerprint(dir); fp != afterMod {
		t.Error("非 .json 文件不应影响指纹")
	}
}

// TestDirFingerprintUnreadable 目录不可读返回空串（调用方据此跳过本轮）。
func TestDirFingerprintUnreadable(t *testing.T) {
	if _, ok := dirFingerprint(filepath.Join(t.TempDir(), "nope")); ok {
		t.Error("不可读目录应返回 ok=false")
	}
}

// TestStartAuthDirWatchNoopOnBadDir 目录不可读时返回可用的 no-op 停止函数，不 panic。
func TestStartAuthDirWatchNoopOnBadDir(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()

	stop := p.StartAuthDirWatch(filepath.Join(t.TempDir(), "missing"))
	stop()
	stop() // 幂等：重复调用不 panic
}

// TestReloadAuthDirAddsAccount 目录新增凭证后热加载进池。
func TestReloadAuthDirAddsAccount(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "u1", "一号")

	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.SyncToDir(auths)

	if n := len(p.AvailableUIDs()); n != 1 {
		t.Fatalf("初始账号数=%d want 1", n)
	}

	// 新增第二个账号并热加载
	writeAuthFile(t, dir, "u2", "二号")
	p.reloadAuthDir(dir)

	uids := p.AvailableUIDs()
	if len(uids) != 2 {
		t.Fatalf("热加载后账号数=%d want 2 (%v)", len(uids), uids)
	}
	found := false
	for _, u := range uids {
		if u == "u2" {
			found = true
		}
	}
	if !found {
		t.Errorf("新账号 u2 未进池: %v", uids)
	}
}

// TestReloadAuthDirPreservesState 热加载不得重置既有账号的冷却/统计状态。
//
// 这是本特性最关键的契约：upsertLocked 对已存在账号只换凭证。若误写成"重建条目"，
// 用户每次加号都会把全池冷却清空（等于绕过限流惩罚），是严重回归。
func TestReloadAuthDirPreservesState(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "u1", "一号")

	auths, _ := auth.LoadDir(dir)
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.SyncToDir(auths)

	// 给 u1 制造状态：软冷却 + 计数
	p.Cooldown("u1", CoolSoft, time.Hour, "test")
	p.NoteError("u1")

	before := statusByUID(t, p, "u1")
	if !before.Cooling {
		t.Fatal("前置：u1 应处于冷却")
	}

	// 刷新 u1 凭证（同名覆盖）+ 新增 u2，然后热加载
	writeAuthFile(t, dir, "u1", "一号新凭证")
	writeAuthFile(t, dir, "u2", "二号")
	p.reloadAuthDir(dir)

	after := statusByUID(t, p, "u1")
	if !after.Cooling {
		t.Error("热加载不得清除既有账号的冷却状态")
	}
	if after.CoolKind != before.CoolKind {
		t.Errorf("cool_kind 被改变: %q → %q", before.CoolKind, after.CoolKind)
	}
	if after.SuccessCount != before.SuccessCount || after.ErrTotal != before.ErrTotal {
		t.Errorf("计数被重置: succ %d→%d, err %d→%d",
			before.SuccessCount, after.SuccessCount, before.ErrTotal, after.ErrTotal)
	}
}

// TestReloadAuthDirRemovesDeleted 文件被删除的账号应从池中剔除。
func TestReloadAuthDirRemovesDeleted(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "u1", "一号")
	f2 := writeAuthFile(t, dir, "u2", "二号")

	auths, _ := auth.LoadDir(dir)
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.SyncToDir(auths)
	if n := len(p.AvailableUIDs()); n != 2 {
		t.Fatalf("前置：账号数=%d want 2", n)
	}

	if err := os.Remove(f2); err != nil {
		t.Fatal(err)
	}
	p.reloadAuthDir(dir)

	if n := len(p.AvailableUIDs()); n != 1 {
		t.Errorf("删除文件后账号数=%d want 1", n)
	}
}

// statusByUID 从 List() 取指定账号状态（同包测试辅助）。
func statusByUID(t *testing.T, p *Pool, uid string) Status {
	t.Helper()
	for _, st := range p.List() {
		if st.UID == uid {
			return st
		}
	}
	t.Fatalf("账号 %s 不在池中", uid)
	return Status{}
}

// TestReloadAuthDirSkipsWhenFileUnparseable 半写安全闸（评审两轮修复）：目录内存在
// 读失败/解析失败的文件时，本轮 reload **不得做删除**——否则坏文件对应账号会被
// 当成"已删除"剔除并落盘，state.json 里的 credits/冷却/熔断计数被永久抹掉；
// 但**新增/更新仍要即时生效**（只 UpsertOnly），否则一个长期残留的坏文件会让
// 之后所有新增账号的热加载一并失效（只能重启）。
// 触发场景：sync-auths.sh 的 cp（非原子）写凭证，watcher 在 5s tick 读到半截 JSON。
func TestReloadAuthDirSkipsWhenFileUnparseable(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "u1", "一号")
	writeAuthFile(t, dir, "u2", "二号")

	auths, _ := auth.LoadDir(dir)
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.SyncToDir(auths)
	p.SetCredits("u2", 4242)
	if n := len(p.AvailableUIDs()); n != 2 {
		t.Fatalf("前置：账号数=%d want 2", n)
	}

	// 模拟半写入：u2 文件被截断成半截 JSON。
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-u2.json"), []byte(`{"account":{"uid":"u2"`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.reloadAuthDir(dir)

	if n := len(p.AvailableUIDs()); n != 2 {
		t.Fatalf("有坏文件时不得删除（u2 会被误删）：账号数=%d want 2", n)
	}
	if st := statusByUID(t, p, "u2"); st.Credits != 4242 {
		t.Errorf("坏文件轮次后 u2 credits=%d want 4242（状态不得被抹）", st.Credits)
	}

	// 关键：坏文件在场时新增账号仍须热加载（只增改不删除），否则热加载被永久锁死。
	writeAuthFile(t, dir, "u3", "三号")
	p.reloadAuthDir(dir)
	if n := len(p.AvailableUIDs()); n != 3 {
		t.Fatalf("坏文件在场时新增账号仍应热加载：账号数=%d want 3", n)
	}
}

// TestReloadAuthDirCorruptThenHealed 半写恢复：坏文件修复后（下一轮指纹变化重扫）
// 同步恢复正常，且此前跳过的删除仍能生效。
func TestReloadAuthDirCorruptThenHealed(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "u1", "一号")
	f2 := writeAuthFile(t, dir, "u2", "二号")

	auths, _ := auth.LoadDir(dir)
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.SyncToDir(auths)

	// 第一轮：坏文件 → 跳过同步。
	if err := os.WriteFile(f2, []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.reloadAuthDir(dir)
	if n := len(p.AvailableUIDs()); n != 2 {
		t.Fatalf("坏文件轮次账号数=%d want 2（跳过同步）", n)
	}

	// 第二轮：文件删除（等价于"坏文件被清理"）→ 同步恢复，u2 正常剔除。
	if err := os.Remove(f2); err != nil {
		t.Fatal(err)
	}
	p.reloadAuthDir(dir)
	if n := len(p.AvailableUIDs()); n != 1 {
		t.Errorf("坏文件清理后账号数=%d want 1（同步恢复）", n)
	}
}
