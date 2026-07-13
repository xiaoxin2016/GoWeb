// Package server 实现 HTTP 路由、鉴权中间件与页面渲染。
package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/xiaoxin2016/goweb/internal/auth"
	"github.com/xiaoxin2016/goweb/internal/config"
	"github.com/xiaoxin2016/goweb/internal/storage"
	"github.com/xiaoxin2016/goweb/web"
)

const sessionCookie = "goweb_session"

// Server 应用 HTTP 服务。
type Server struct {
	cfg   *config.Store
	codes *auth.CodeManager
	tpl   *template.Template
	mux   *http.ServeMux

	s3mu  sync.Mutex
	s3    *storage.Client
	s3rev int64 // 构建 s3 客户端时的配置修订号
}

// New 构建服务并注册路由。
func New(cfg *config.Store) (*Server, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"humanSize": humanSize,
		"fmtTime":   fmtTime,
	}).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:   cfg,
		codes: auth.NewCodeManager(),
		tpl:   tpl,
		mux:   http.NewServeMux(),
		s3rev: -1,
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	m := s.mux
	m.Handle("GET /static/", http.FileServerFS(web.FS))

	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/files/", http.StatusFound)
	})

	// 登录
	m.HandleFunc("GET /login", s.handleLoginPage)
	m.HandleFunc("POST /api/auth/send-code", s.handleSendCode)
	m.HandleFunc("POST /api/auth/verify", s.handleVerify)
	m.HandleFunc("POST /api/auth/logout", s.handleLogout)

	// 文件浏览与操作（需要登录）
	m.HandleFunc("GET /files/", s.requireUser(s.handleBrowse))
	m.HandleFunc("POST /api/upload", s.requireUserAPI(s.handleUpload))
	m.HandleFunc("GET /api/download", s.requireUser(s.handleDownload))
	m.HandleFunc("POST /api/delete", s.requireUserAPI(s.handleDelete))
	m.HandleFunc("POST /api/mkdir", s.requireUserAPI(s.handleMkdir))

	// 控制台（需要管理员；初始化模式下开放）
	m.HandleFunc("GET /console", s.requireAdmin(s.handleConsole))
	m.HandleFunc("POST /console/s3", s.requireAdmin(s.handleSaveS3))
	m.HandleFunc("POST /console/smtp", s.requireAdmin(s.handleSaveSMTP))
	m.HandleFunc("POST /console/auth", s.requireAdmin(s.handleSaveAuth))
	m.HandleFunc("POST /api/console/test-s3", s.requireAdminAPI(s.handleTestS3))
	m.HandleFunc("POST /api/console/test-smtp", s.requireAdminAPI(s.handleTestSMTP))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// s3Client 返回与当前配置匹配的 S3 客户端；配置变化后自动重建。
func (s *Server) s3Client() (*storage.Client, error) {
	s.s3mu.Lock()
	defer s.s3mu.Unlock()
	rev := s.cfg.Rev()
	if s.s3 == nil || s.s3rev != rev {
		cli, err := storage.New(s.cfg.Get().S3)
		if err != nil {
			return nil, err
		}
		s.s3, s.s3rev = cli, rev
	}
	return s.s3, nil
}

// ---- 会话 ----

// currentUser 返回当前登录用户邮箱；未登录或不再被允许时返回 ""。
func (s *Server) currentUser(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	email, ok := auth.ParseToken(s.cfg.Secret(), c.Value)
	if !ok || !s.cfg.Get().IsAllowed(email) {
		return ""
	}
	return email
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// ---- 中间件 ----

// requireUser 页面版：未登录时跳转登录页；初始化模式下跳转控制台。
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Get().SetupMode() {
			http.Redirect(w, r, "/console", http.StatusFound)
			return
		}
		if s.currentUser(r) == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// requireUserAPI 接口版：未登录时返回 401 JSON。
func (s *Server) requireUserAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.currentUser(r) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录或会话已过期"})
			return
		}
		next(w, r)
	}
}

func (s *Server) isAdminReq(r *http.Request) bool {
	cfg := s.cfg.Get()
	if cfg.SetupMode() {
		return true // 初始化模式：控制台开放，用于首次配置
	}
	email := s.currentUser(r)
	return email != "" && cfg.IsAdmin(email)
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdminReq(r) {
			if s.currentUser(r) == "" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			http.Error(w, "需要管理员权限", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) requireAdminAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdminReq(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "需要管理员权限"})
			return
		}
		next(w, r)
	}
}

// ---- 工具函数 ----

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("渲染模板 %s 失败: %v", name, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// cleanDir 校验并规范化目录相对路径：返回 "" 或以 "/" 结尾的路径。
func cleanDir(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	if err := checkSegments(p); err != nil {
		return "", err
	}
	return p + "/", nil
}

// cleanFile 校验文件相对路径（不能以 "/" 结尾）。
func cleanFile(p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	if p == "" || strings.HasSuffix(p, "/") {
		return "", fmt.Errorf("非法文件路径")
	}
	if err := checkSegments(p); err != nil {
		return "", err
	}
	return p, nil
}

func checkSegments(p string) error {
	if strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("路径包含非法字符")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("非法路径")
		}
	}
	return nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
