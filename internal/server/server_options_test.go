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

// 重命名仅管理员：普通用户被拒，管理员放行
func TestRenameAdminOnly(t *testing.T) {
	srv := newTestServer(t, Options{IgnoreEmail: true})
	userCk := login(t, srv, "user@test.com")
	adminCk := login(t, srv, "admin@test.com")

	body := `{"path":"docs/a.txt","name":"b.txt"}`
	rec := postJSON(t, srv, "/api/rename", body, userCk)
	if rec.Code != http.StatusForbidden {
		t.Errorf("普通用户重命名应返回 403，实际 %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "仅管理员") {
		t.Errorf("应说明仅管理员可操作，实际 %s", rec.Body)
	}

	// 管理员越过权限检查后进入存储阶段（此处 S3 未配置，故为 503 而非 403）
	rec = postJSON(t, srv, "/api/rename", body, adminCk)
	if rec.Code == http.StatusForbidden {
		t.Errorf("管理员不应被权限拦截，实际 %d %s", rec.Code, rec.Body)
	}

	// 普通用户的上传/删除不受影响，仍只受目录只读约束
	rec = postJSON(t, srv, "/api/delete", `{"paths":["docs/a.txt"]}`, userCk)
	if rec.Code == http.StatusForbidden {
		t.Errorf("普通用户删除不应被拒，实际 %d %s", rec.Code, rec.Body)
	}
}
