package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaoxin2016/goweb/internal/config"
)

// newTestServer 构建一个已完成初始化（有管理员、允许 *@test.com）的服务实例。
func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	store, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) {
		c.Auth.AdminEmails = []string{"admin@test.com"}
		c.Auth.AllowedEmails = []string{"*@test.com"}
		// 指向不可达端口：--ignore-email 生效时不应触碰 SMTP
		c.SMTP = config.SMTPConfig{Host: "127.0.0.1", Port: 1, From: "noreply@test.com", Encryption: "none"}
	}); err != nil {
		t.Fatal(err)
	}
	srv, err := New(store, opts)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func postJSON(t *testing.T, srv *Server, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// login 走完整的发码 + 校验流程，返回会话 Cookie。
// 依赖 --ignore-email：验证码不经 SMTP，直接从 CodeManager 取。
func login(t *testing.T, srv *Server, email string) *http.Cookie {
	t.Helper()
	if rec := postJSON(t, srv, "/api/auth/send-code",
		`{"email":"`+email+`"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("发码失败: %d %s", rec.Code, rec.Body)
	}
	code := srv.codes.Peek(email)
	if code == "" {
		t.Fatalf("%s 未生成验证码", email)
	}
	rec := postJSON(t, srv, "/api/auth/verify",
		`{"email":"`+email+`","code":"`+code+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("未拿到会话 Cookie")
	return nil
}

// --ignore-email 下 SMTP 不可达也能发码成功，且登录流程照常走完
func TestIgnoreEmailSkipsSMTP(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})

	rec := postJSON(t, srv, "/api/auth/send-code", `{"email":"user@test.com"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("SMTP 不可达时仍应成功，实际 %d %s", rec.Code, rec.Body)
	}
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["message"] == "" {
		t.Errorf("响应应与正常发送一致，实际 %v", resp)
	}
	// 验证码已真实签发，可直接凭它完成登录（此处不能再次发码：会触发重发限流）
	code := srv.codes.Peek("user@test.com")
	if code == "" {
		t.Fatal("--ignore-email 下仍应签发验证码")
	}
	rec = postJSON(t, srv, "/api/auth/verify",
		`{"email":"user@test.com","code":"`+code+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("凭控制台打印的验证码应能登录，实际 %d %s", rec.Code, rec.Body)
	}
	var got *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			got = c
		}
	}
	if got == nil || got.Value == "" {
		t.Error("应签发会话令牌")
	}
}

// 未开启该参数时仍走 SMTP：不可达的服务器应导致发送失败
func TestWithoutIgnoreEmailUsesSMTP(t *testing.T) {
	srv := newTestServer(t, Options{})
	rec := postJSON(t, srv, "/api/auth/send-code", `{"email":"user2@test.com"}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("SMTP 不可达时应报发送失败，实际 %d %s", rec.Code, rec.Body)
	}
}

// adminRoutes 列出全部管理员端点。新增管理员功能时应同步加入本表，
// 由下面的用例统一保证“对非管理员不可见”。
var adminRoutes = []struct {
	method, path, body string
}{
	{http.MethodGet, "/console", ""},
	{http.MethodGet, "/console/audit", ""},
	{http.MethodPost, "/console/s3", ""},
	{http.MethodPost, "/console/smtp", ""},
	{http.MethodPost, "/console/auth", ""},
	{http.MethodPost, "/console/dirperm", ""},
	{http.MethodPost, "/console/notice", ""},
	{http.MethodPost, "/console/syslog", ""},
	{http.MethodPost, "/api/console/test-s3", `{}`},
	{http.MethodPost, "/api/console/test-smtp", `{}`},
	{http.MethodPost, "/api/console/test-syslog", `{}`},
	{http.MethodPost, "/api/rename", `{"path":"docs/a.txt","name":"b.txt"}`},
}

func do(t *testing.T, srv *Server, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// 所有管理员端点对已登录的普通用户都必须“不存在”：
// 响应需与未知路由逐字一致，无法据此判断该功能是否存在。
func TestAdminRoutesInvisibleToNormalUser(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	userCk := login(t, srv, "user@test.com")

	base := do(t, srv, http.MethodGet, "/definitely-not-a-real-path", "", userCk)
	if base.Code != http.StatusNotFound {
		t.Fatalf("基准用例应为 404，实际 %d", base.Code)
	}

	for _, rt := range adminRoutes {
		rec := do(t, srv, rt.method, rt.path, rt.body, userCk)
		if rec.Code != base.Code {
			t.Errorf("%s %s：状态码应为 %d，实际 %d %s", rt.method, rt.path, base.Code, rec.Code, rec.Body)
		}
		if rec.Body.String() != base.Body.String() {
			t.Errorf("%s %s：响应体应与未知路由一致\n  实际 %q\n  基准 %q",
				rt.method, rt.path, rec.Body.String(), base.Body.String())
		}
		if got, want := rec.Header().Get("Content-Type"), base.Header().Get("Content-Type"); got != want {
			t.Errorf("%s %s：Content-Type 应一致，实际 %q 基准 %q", rt.method, rt.path, got, want)
		}
	}
}

// 管理员接口对未登录者同样不可见（不能用 401 暴露端点存在）
func TestAdminAPIsInvisibleWhenLoggedOut(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	base := do(t, srv, http.MethodPost, "/api/definitely-not-real", `{}`, nil)

	for _, rt := range adminRoutes {
		if !strings.HasPrefix(rt.path, "/api/") {
			continue // 页面端点未登录时跳转登录页，属正常鉴权流程
		}
		rec := do(t, srv, rt.method, rt.path, rt.body, nil)
		if rec.Code != http.StatusNotFound || rec.Body.String() != base.Body.String() {
			t.Errorf("%s %s：未登录时应与未知路由一致，实际 %d %s",
				rt.method, rt.path, rec.Code, rec.Body)
		}
	}
}

// 畸形/空 body 也不得因解析失败先返回 400 而暴露端点存在
func TestAdminRoutesInvisibleWithBadBody(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	userCk := login(t, srv, "user@test.com")

	for _, rt := range adminRoutes {
		for _, body := range []string{"", "{{{", strings.Repeat("x", 4096)} {
			rec := do(t, srv, rt.method, rt.path, body, userCk)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s（body=%.8q）：应为 404，实际 %d %s",
					rt.method, rt.path, body, rec.Code, rec.Body)
			}
		}
	}
}

// 响应中不得出现任何暗示管理员功能存在的字样
func TestAdminDenialLeaksNothing(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	userCk := login(t, srv, "user@test.com")

	for _, rt := range adminRoutes {
		body := do(t, srv, rt.method, rt.path, rt.body, userCk).Body.String()
		for _, word := range []string{"管理员", "权限", "重命名", "rename", "console", "admin"} {
			if strings.Contains(body, word) {
				t.Errorf("%s %s 的响应体含敏感字样 %q：%s", rt.method, rt.path, word, body)
			}
		}
	}
}

// 初始化模式（尚无管理员）下：控制台开放以便首次配置，
// 但控制台之外的管理员功能与审计日志仍然不可用。
func TestSetupModeOnlyOpensConsoleConfig(t *testing.T) {
	store, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(store, Options{IgnoreEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	if !store.Get().SetupMode() {
		t.Fatal("新实例应处于初始化模式")
	}

	if rec := do(t, srv, http.MethodGet, "/console", "", nil); rec.Code != http.StatusOK {
		t.Errorf("初始化模式下控制台应开放，实际 %d", rec.Code)
	}
	// 文件类管理员操作不应因初始化模式而对匿名者敞开
	if rec := do(t, srv, http.MethodPost, "/api/rename",
		`{"path":"a.txt","name":"b.txt"}`, nil); rec.Code != http.StatusNotFound {
		t.Errorf("初始化模式下重命名仍应不可用，实际 %d %s", rec.Code, rec.Body)
	}
	// 审计日志含既有用户活动，不对初始化模式开放
	if rec := do(t, srv, http.MethodGet, "/console/audit", "", nil); rec.Code == http.StatusOK {
		t.Errorf("初始化模式下审计页不应开放，实际 %d", rec.Code)
	}
}

// 管理员可以正常使用重命名（不会被权限拦下）
func TestRenameAllowedForAdmin(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	adminCk := login(t, srv, "admin@test.com")

	rec := postJSON(t, srv, "/api/rename", `{"path":"docs/a.txt","name":"b.txt"}`, adminCk)
	// 越过权限检查后进入存储阶段（此处 S3 未配置，故为 503）
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusForbidden {
		t.Errorf("管理员不应被权限拦截，实际 %d %s", rec.Code, rec.Body)
	}
}

// 普通用户的上传/删除权限不受本次调整影响
func TestNonAdminKeepsOtherWrites(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	userCk := login(t, srv, "user@test.com")

	rec := postJSON(t, srv, "/api/delete", `{"paths":["docs/a.txt"]}`, userCk)
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound {
		t.Errorf("普通用户删除不应被拒，实际 %d %s", rec.Code, rec.Body)
	}
	rec = postJSON(t, srv, "/api/mkdir", `{"dir":"","name":"newdir"}`, userCk)
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound {
		t.Errorf("普通用户新建文件夹不应被拒，实际 %d %s", rec.Code, rec.Body)
	}
}

// clientIP 只应采信形如 IP 的 X-Forwarded-For，否则回退连接地址：
// 该头由客户端可控，未经校验会被用来伪造日志与审计记录。
func TestClientIPRejectsForgedXFF(t *testing.T) {
	cases := []struct {
		name, xff, want string
	}{
		{"无该头", "", "192.0.2.10"},
		{"合法 IPv4", "203.0.113.5", "203.0.113.5"},
		{"合法 IPv6", "2001:db8::1", "2001:db8::1"},
		{"代理链取首个", "203.0.113.5, 10.0.0.1, 10.0.0.2", "203.0.113.5"},
		{"首个含空白", "  203.0.113.5 , 10.0.0.1", "203.0.113.5"},
		{"伪造日志行", "1.2.3.4\tFAKE] 拒绝非管理员访问 GET /admin", "192.0.2.10"},
		{"非 IP 文本", "not-an-ip", "192.0.2.10"},
		{"空值", "   ", "192.0.2.10"},
		{"首个为空", ", 203.0.113.5", "192.0.2.10"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.10:54321"
		if tc.xff != "" {
			req.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := clientIP(req); got != tc.want {
			t.Errorf("%s：clientIP = %q，期望 %q", tc.name, got, tc.want)
		}
	}
}

// 日志中的身份描述必须区分未登录、令牌无效与已登录普通用户
func TestRequesterDescDistinguishesStates(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	userCk := login(t, srv, "user@test.com")

	noCookie := httptest.NewRequest(http.MethodPost, "/api/rename", nil)
	if got := srv.requesterDesc(noCookie); !strings.Contains(got, "未登录") {
		t.Errorf("无 Cookie 应描述为未登录，实际 %q", got)
	}

	forged := httptest.NewRequest(http.MethodPost, "/api/rename", nil)
	forged.AddCookie(&http.Cookie{Name: sessionCookie, Value: "forged.garbage.token"})
	got := srv.requesterDesc(forged)
	if !strings.Contains(got, "无效") {
		t.Errorf("伪造令牌应被单独标注，实际 %q", got)
	}
	if strings.Contains(got, "未登录") {
		t.Errorf("伪造令牌不应与未登录混为一谈，实际 %q", got)
	}

	logged := httptest.NewRequest(http.MethodPost, "/api/rename", nil)
	logged.AddCookie(userCk)
	if got := srv.requesterDesc(logged); got != "user@test.com" {
		t.Errorf("已登录用户应记录邮箱，实际 %q", got)
	}
}
