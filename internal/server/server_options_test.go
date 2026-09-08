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

// 重命名仅管理员可用，且对其他人不可见：响应必须与未知路由完全一致，
// 无法据此判断该接口是否存在。
func TestRenameInvisibleToNonAdmin(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	userCk := login(t, srv, "user@test.com")

	// 基准：一个确实不存在的接口
	notFound := postJSON(t, srv, "/api/definitely-not-a-real-endpoint",
		`{"path":"docs/a.txt","name":"b.txt"}`, userCk)
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("基准用例应为 404，实际 %d", notFound.Code)
	}

	body := `{"path":"docs/a.txt","name":"b.txt"}`
	cases := []struct {
		name   string
		cookie *http.Cookie
		body   string
	}{
		{"普通用户", userCk, body},
		{"未登录", nil, body},
		{"普通用户 + 畸形 JSON", userCk, `{{{`}, // 不得因解析失败而返回 400
		{"普通用户 + 空 body", userCk, ``},
		{"未登录 + 畸形 JSON", nil, `{{{`},
	}
	for _, tc := range cases {
		rec := postJSON(t, srv, "/api/rename", tc.body, tc.cookie)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s：应返回 404，实际 %d %s", tc.name, rec.Code, rec.Body)
		}
		if rec.Body.String() != notFound.Body.String() {
			t.Errorf("%s：响应体应与未知路由一致\n  实际 %q\n  基准 %q",
				tc.name, rec.Body.String(), notFound.Body.String())
		}
		if got, want := rec.Header().Get("Content-Type"), notFound.Header().Get("Content-Type"); got != want {
			t.Errorf("%s：Content-Type 应与未知路由一致，实际 %q 基准 %q", tc.name, got, want)
		}
	}
	// 响应里不得出现任何暗示该功能存在的字样
	rec := postJSON(t, srv, "/api/rename", body, userCk)
	for _, word := range []string{"重命名", "管理员", "rename"} {
		if strings.Contains(rec.Body.String(), word) {
			t.Errorf("响应体不应包含 %q：%s", word, rec.Body)
		}
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
