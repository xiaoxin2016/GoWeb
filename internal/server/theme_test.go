package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/xiaoxin2016/goweb/internal/config"
)

func postForm(t *testing.T, srv *Server, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func loginPageTheme(t *testing.T, srv *Server) string {
	t.Helper()
	rec := do(t, srv, http.MethodGet, "/login", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录页应为 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	const attr = `data-theme="`
	i := strings.Index(body, attr)
	if i < 0 {
		t.Fatal("登录页缺少 data-theme 属性")
	}
	rest := body[i+len(attr):]
	return rest[:strings.IndexByte(rest, '"')]
}

// 未配置主题时为浅色；管理员切换后，所有页面（含未登录可见的登录页）即时生效
func TestThemeSwitch(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	if got := loginPageTheme(t, srv); got != config.ThemeLight {
		t.Fatalf("默认主题应为 %q，实际 %q", config.ThemeLight, got)
	}

	adminCk := login(t, srv, "admin@test.com")
	rec := postForm(t, srv, "/console/theme", url.Values{"theme": {"navy"}}, adminCk)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "msg=") {
		t.Fatalf("保存主题应重定向并带成功提示，实际 %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if got := srv.cfg.Get().Theme; got != config.ThemeNavy {
		t.Fatalf("配置中的主题应为 navy，实际 %q", got)
	}
	if got := loginPageTheme(t, srv); got != config.ThemeNavy {
		t.Errorf("登录页主题应为 navy，实际 %q", got)
	}
	for _, path := range []string{"/console", "/console/audit"} {
		body := do(t, srv, http.MethodGet, path, "", adminCk).Body.String()
		if !strings.Contains(body, `data-theme="navy"`) {
			t.Errorf("%s 未应用 navy 主题", path)
		}
	}
	console := do(t, srv, http.MethodGet, "/console", "", adminCk).Body.String()
	if !strings.Contains(console, `value="navy" checked`) {
		t.Error("控制台应把当前主题标为选中")
	}
}

// 非法取值被拒绝且不改动配置；模板只会拿到已知主题，杜绝属性注入
func TestThemeRejectsUnknownValue(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	adminCk := login(t, srv, "admin@test.com")

	for _, v := range []string{"", "dark", `navy" onload="x`, "NAVY"} {
		rec := postForm(t, srv, "/console/theme", url.Values{"theme": {v}}, adminCk)
		if !strings.Contains(rec.Header().Get("Location"), "err=") {
			t.Errorf("主题 %q 应被拒绝，实际 %d %s", v, rec.Code, rec.Header().Get("Location"))
		}
		if got := srv.cfg.Get().Theme; got != "" {
			t.Fatalf("非法主题 %q 不应写入配置，实际 %q", v, got)
		}
	}

	// 配置文件被手工改成未知值时，页面退回默认主题
	if err := srv.cfg.Update(func(c *config.Config) { c.Theme = "bogus" }); err != nil {
		t.Fatal(err)
	}
	if got := loginPageTheme(t, srv); got != config.ThemeLight {
		t.Errorf("未知主题应退回 %q，实际 %q", config.ThemeLight, got)
	}
}

// 普通用户无法切换主题（端点对其不可见，配置不变）
func TestThemeAdminOnly(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	userCk := login(t, srv, "user@test.com")
	rec := postForm(t, srv, "/console/theme", url.Values{"theme": {"navy"}}, userCk)
	if rec.Code != http.StatusNotFound {
		t.Errorf("普通用户应得到 404，实际 %d", rec.Code)
	}
	if got := srv.cfg.Get().Theme; got != "" {
		t.Errorf("普通用户不应改动主题，实际 %q", got)
	}
}
