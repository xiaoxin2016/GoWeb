package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func validEmailForTest(e string) bool {
	return strings.Count(e, "@") == 1 && !strings.ContainsAny(e, " \r\n")
}

func TestSessionCreateLookupDelete(t *testing.T) {
	s := NewSessionStore()
	tok, exp, err := s.Create("User@Example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !validSessionToken(tok) {
		t.Fatalf("令牌格式不符合预期: %q", tok)
	}
	if d := time.Until(exp); d < 59*time.Minute || d > time.Hour {
		t.Fatalf("到期时间应约为 1 小时后，实际 %v", d)
	}
	if email, ok := s.Lookup(tok); !ok || email != "user@example.com" {
		t.Fatalf("Lookup = %q, %v", email, ok)
	}
	if _, ok := s.Lookup(tok + "x"); ok {
		t.Fatal("不存在的令牌不应通过")
	}
	if _, ok := s.Lookup(""); ok {
		t.Fatal("空令牌不应通过")
	}
	s.Delete(tok)
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("退出登录后令牌应立即失效")
	}

	// 两次登录得到不同令牌
	a, _, _ := s.Create("a@b.com", time.Hour)
	b, _, _ := s.Create("a@b.com", time.Hour)
	if a == b {
		t.Fatal("每次登录都应生成新令牌")
	}
}

func TestSessionExpiry(t *testing.T) {
	s := NewSessionStore()
	tok, _, _ := s.Create("a@b.com", -time.Second)
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("过期会话不应通过")
	}
	if s.Len() != 0 {
		t.Fatal("过期会话在查询时应被删除")
	}
}

func TestSessionSaveRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s := NewSessionStore()
	tokA, expA, _ := s.Create("a@b.com", 2*time.Hour)
	tokB, _, _ := s.Create("b@b.com", time.Hour)
	s.Create("expired@b.com", -time.Minute) // 已过期的不应写入

	n, err := s.Save(path)
	if err != nil || n != 2 {
		t.Fatalf("Save = %d, %v；应保存 2 个有效会话", n, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("快照权限应为 0600，实际 %04o", fi.Mode().Perm())
		}
	}

	r := NewSessionStore()
	res, err := r.Restore(path, 24*time.Hour, validEmailForTest)
	if err != nil || !res.Found || res.Restored != 2 {
		t.Fatalf("Restore = %+v, %v", res, err)
	}
	if email, ok := r.Lookup(tokA); !ok || email != "a@b.com" {
		t.Fatal("恢复后原令牌应继续有效")
	}
	if _, ok := r.Lookup(tokB); !ok {
		t.Fatal("恢复后原令牌应继续有效")
	}
	// 沿用原有效期，不因重启而续期
	r.mu.Lock()
	gotExp := r.m[tokA].expires
	r.mu.Unlock()
	if gotExp.Unix() != expA.Unix() {
		t.Fatalf("恢复的会话应沿用原有效期 %v，实际 %v", expA, gotExp)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("快照读回后应立即删除")
	}

	// 再启动一次：没有快照，什么也不恢复
	res, err = NewSessionStore().Restore(path, 24*time.Hour, validEmailForTest)
	if err != nil || res.Found {
		t.Fatalf("无快照时应静默返回，实际 %+v, %v", res, err)
	}
}

func TestSessionSaveNoSessionsLeavesNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := NewSessionStore().Save(path); err != nil || n != 0 {
		t.Fatalf("Save = %d, %v", n, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("没有有效会话时不应留下快照文件（含旧文件）")
	}
}

func TestSessionRestoreSkipsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	writeSnap(t, path, 0o600, []sessionSnapItem{
		{Token: strings.Repeat("a", 43), Email: "a@b.com", Expires: time.Now().Add(time.Hour).Unix()},
		{Token: strings.Repeat("b", 43), Email: "b@b.com", Expires: time.Now().Add(-time.Hour).Unix()},
	})
	res, err := NewSessionStore().Restore(path, 24*time.Hour, validEmailForTest)
	if err != nil || res.Restored != 1 || res.Expired != 1 {
		t.Fatalf("应恢复 1 个、跳过 1 个过期会话，实际 %+v, %v", res, err)
	}
}

// 快照任何一处不可信，都整份丢弃，一个会话都不恢复，并且删除文件
func TestSessionRestoreRejects(t *testing.T) {
	good := sessionSnapItem{Token: strings.Repeat("a", 43), Email: "a@b.com", Expires: time.Now().Add(time.Hour).Unix()}
	cases := []struct {
		name  string
		perm  os.FileMode
		items []sessionSnapItem
		raw   string
	}{
		{name: "非法 JSON（写到一半）", perm: 0o600, raw: `{"sessions":[{"token":`},
		{name: "令牌长度不对", perm: 0o600, items: []sessionSnapItem{good, {Token: "short", Email: "b@b.com", Expires: good.Expires}}},
		{name: "令牌含非法字符", perm: 0o600, items: []sessionSnapItem{good, {Token: strings.Repeat("a", 42) + "/", Email: "b@b.com", Expires: good.Expires}}},
		{name: "邮箱不合规", perm: 0o600, items: []sessionSnapItem{good, {Token: strings.Repeat("c", 43), Email: "x\r\ny@b.com", Expires: good.Expires}}},
		{name: "有效期超出会话时长上限", perm: 0o600, items: []sessionSnapItem{good, {Token: strings.Repeat("d", 43), Email: "d@b.com", Expires: time.Now().Add(48 * time.Hour).Unix()}}},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			name  string
			perm  os.FileMode
			items []sessionSnapItem
			raw   string
		}{name: "权限过宽（他人可读写）", perm: 0o644, items: []sessionSnapItem{good}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.json")
			if tc.raw != "" {
				if err := os.WriteFile(path, []byte(tc.raw), tc.perm); err != nil {
					t.Fatal(err)
				}
			} else {
				writeSnap(t, path, tc.perm, tc.items)
			}
			s := NewSessionStore()
			res, err := s.Restore(path, 24*time.Hour, validEmailForTest)
			if err == nil {
				t.Fatal("应拒绝该快照")
			}
			if res.Restored != 0 || s.Len() != 0 {
				t.Fatalf("拒绝时不应恢复任何会话，实际恢复 %d 个", s.Len())
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("被拒绝的快照也应删除")
			}
		})
	}
}

func TestSessionRestoreRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需要特权")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.json")
	writeSnap(t, target, 0o600, []sessionSnapItem{{Token: strings.Repeat("a", 43), Email: "a@b.com", Expires: time.Now().Add(time.Hour).Unix()}})
	link := filepath.Join(dir, "sessions.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s := NewSessionStore()
	if _, err := s.Restore(link, 24*time.Hour, validEmailForTest); err == nil || s.Len() != 0 {
		t.Fatal("符号链接形式的快照应被拒绝")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("只应删除链接本身，不应删除链接指向的文件")
	}
}

func writeSnap(t *testing.T, path string, perm os.FileMode, items []sessionSnapItem) {
	t.Helper()
	data, err := json.Marshal(sessionSnapshot{SavedAt: time.Now(), Sessions: items})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil { // 不受 umask 影响
		t.Fatal(err)
	}
}
