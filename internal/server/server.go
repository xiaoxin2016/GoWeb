// Package server 实现 HTTP 路由、鉴权中间件与页面渲染。
package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/xiaoxin2016/goweb/internal/audit"
	"github.com/xiaoxin2016/goweb/internal/auth"
	"github.com/xiaoxin2016/goweb/internal/config"
	"github.com/xiaoxin2016/goweb/internal/storage"
	"github.com/xiaoxin2016/goweb/web"
)

const sessionCookie = "goweb_session"

// Options 启动参数。
type Options struct {
	// IgnoreEmail 为 true 时不经 SMTP 投递，验证码直接打印到服务端控制台，
	// 登录流程其余环节不变。用于未配置邮件服务的内网部署与排障。
	IgnoreEmail bool
}

// Server 应用 HTTP 服务。
type Server struct {
	cfg   *config.Store
	codes *auth.CodeManager
	tpl   *template.Template
	mux   *http.ServeMux
	audit *audit.Logger
	opts  Options

	s3mu  sync.Mutex
	s3    *storage.Client
	s3rev int64 // 构建 s3 客户端时的配置修订号
}

// New 构建服务并注册路由。
func New(cfg *config.Store, opts Options) (*Server, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"humanSize":   humanSize,
		"fmtTime":     fmtTime,
		"actionLabel": actionLabel,
	}).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	auditLog, err := audit.New(cfg.DataDir())
	if err != nil {
		return nil, err
	}
	auditLog.Configure(cfg.Get().Syslog)

	s := &Server{
		cfg:   cfg,
		codes: auth.NewCodeManager(),
		tpl:   tpl,
		mux:   http.NewServeMux(),
		audit: auditLog,
		opts:  opts,
		s3rev: -1,
	}
	s.routes()
	return s, nil
}

// auditLog 记录一条审计事件。
func (s *Server) auditLog(r *http.Request, action, path string, opErr error) {
	e := audit.Event{
		User:   s.currentUser(r),
		Action: action,
		Path:   path,
		IP:     clientIP(r),
	}
	if opErr != nil {
		e.Result = opErr.Error()
	}
	s.audit.Log(e)
}

// errReadOnly 普通用户对只读目录执行写操作时的错误。
func errReadOnly(rel string) error {
	return fmt.Errorf("目录 %q 为只读，仅管理员可上传、重命名、删除或新建文件夹",
		config.TopDir(rel))
}

// canWrite 报告当前请求的用户能否对相对路径 rel 执行写操作。
func (s *Server) canWrite(r *http.Request, rel string) bool {
	return s.cfg.Get().CanWrite(s.currentUser(r), rel)
}

// clientIP 返回客户端 IP（优先取反向代理透传的 X-Forwarded-For 首个地址）。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// actionLabel 审计操作类型的中文名。
func actionLabel(action string) string {
	switch action {
	case "login":
		return "登录"
	case "login-fail":
		return "登录失败"
	case "access":
		return "访问目录"
	case "download":
		return "下载"
	case "upload":
		return "上传"
	case "mkdir":
		return "新建文件夹"
	case "delete":
		return "删除"
	case "rename":
		return "重命名"
	case "test":
		return "测试"
	default:
		return action
	}
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
	// 不套 requireUserAPI：未登录时它会返回 401，同样暴露了接口的存在。
	// 权限与可见性由 handleRename 内部统一处理（非管理员一律 404）。
	m.HandleFunc("POST /api/rename", s.handleRename)
	m.HandleFunc("POST /api/mkdir", s.requireUserAPI(s.handleMkdir))

	// 控制台（需要管理员；初始化模式下开放）
	m.HandleFunc("GET /console", s.requireAdmin(s.handleConsole))
	m.HandleFunc("POST /console/s3", s.requireAdmin(s.handleSaveS3))
	m.HandleFunc("POST /console/smtp", s.requireAdmin(s.handleSaveSMTP))
	m.HandleFunc("POST /console/auth", s.requireAdmin(s.handleSaveAuth))
	m.HandleFunc("POST /console/dirperm", s.requireAdmin(s.handleSaveDirPerm))
	m.HandleFunc("POST /console/notice", s.requireAdmin(s.handleSaveNotice))
	m.HandleFunc("POST /console/syslog", s.requireAdmin(s.handleSaveSyslog))
	m.HandleFunc("GET /console/audit", s.requireAdmin(s.handleAuditPage))
	m.HandleFunc("POST /api/console/test-s3", s.requireAdminAPI(s.handleTestS3))
	m.HandleFunc("POST /api/console/test-smtp", s.requireAdminAPI(s.handleTestSMTP))
	m.HandleFunc("POST /api/console/test-syslog", s.requireAdminAPI(s.handleTestSyslog))
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
