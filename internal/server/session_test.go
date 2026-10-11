package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaoxin2016/goweb/internal/config"
)

// loggedIn 用会话 Cookie 访问文件页：已登录时返回 200，未登录时跳转登录页。
func loggedIn(t *testing.T, srv *Server, ck *http.Cookie) bool {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/files/", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	switch rec.Code {
	case http.StatusOK:
		return true
	case http.StatusFound:
		return false
	}
	t.Fatalf("意外的状态码 %d", rec.Code)
	return false
}

// 退出登录会在服务端作废令牌：被人拿到的旧 Cookie 也随之失效
func TestLogoutInvalidatesSessionServerSide(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	ck := login(t, srv, "user@test.com")
	if !loggedIn(t, srv, ck) {
		t.Fatal("登录后应可访问")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(ck)
	srv.ServeHTTP(httptest.NewRecorder(), req)

	if loggedIn(t, srv, ck) {
		t.Fatal("退出登录后，原令牌不应再被接受")
	}
}

// 优雅退出时保存会话、下次启动恢复；快照只用一次
func TestSessionsSurviveGracefulRestart(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	ck := login(t, srv, "user@test.com")
	gone := login(t, srv, "admin@test.com")
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(gone)
	srv.ServeHTTP(httptest.NewRecorder(), req)

	srv.SaveSessions()
	snap := srv.sessionFile()
	if fi, err := os.Stat(snap); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("应生成 0600 的会话快照: %v", err)
	}
	if filepath.Dir(snap) != srv.cfg.DataDir() {
		t.Fatal("快照应位于数据目录")
	}

	// 模拟升级后的新进程：同一数据目录
	srv2, err := New(srv.cfg, Options{IgnoreEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	if !loggedIn(t, srv2, ck) {
		t.Fatal("重启后原会话应继续有效")
	}
	if loggedIn(t, srv2, gone) {
		t.Fatal("已退出登录的会话不应被恢复")
	}
	if _, err := os.Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("快照恢复后应立即删除")
	}

	// 再次启动且上次未优雅退出（如 kill -9）：没有快照，需要重新登录
	srv3, err := New(srv.cfg, Options{IgnoreEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	if loggedIn(t, srv3, ck) {
		t.Fatal("没有快照时不应凭空恢复会话")
	}
}

// 被移出允许名单的用户，即便会话仍在也立即失去访问权限
func TestSessionRevokedWhenRemovedFromAllowList(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	ck := login(t, srv, "user@test.com")
	if err := srv.cfg.Update(func(c *config.Config) { c.Auth.AllowedEmails = nil }); err != nil {
		t.Fatal(err)
	}
	if loggedIn(t, srv, ck) {
		t.Fatal("移出允许名单后应立即失去访问权限")
	}
}
