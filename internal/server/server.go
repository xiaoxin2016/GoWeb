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
	return fmt.Errorf("目录 %q 为只读，仅管理员可上传、删除或新建文件夹",
		config.TopDir(rel))
}

// canWrite 报告当前请求的用户能否对相对路径 rel 执行写操作。
func (s *Server) canWrite(r *http.Request, rel string) bool {
	return s.cfg.Get().CanWrite(s.currentUser(r), rel)
}

// clientIP 返回客户端 IP，优先取反向代理透传的 X-Forwarded-For 首个地址。
//
// 该头由客户端完全可控：未经校验就原样记录，攻击者可借此伪造出足以乱真的
// 日志行嫁祸他人，也会污染审计记录与外发到 rsyslog 的内容。因此只在其首个
// 地址确实能解析为 IP 时才采信，否则回退到真实的连接地址。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := xff
		if i := strings.IndexByte(xff, ','); i >= 0 {
			first = xff[:i]
		}
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip.String()
		}
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
	m.HandleFunc("POST /api/rename", s.requireAdminAPI(adminStrict, s.handleRename))
	m.HandleFunc("POST /api/mkdir", s.requireUserAPI(s.handleMkdir))

	// 控制台配置：初始化模式下开放，供首次配置
	m.HandleFunc("GET /console", s.requireAdmin(setupOpen, s.handleConsole))
	m.HandleFunc("POST /console/s3", s.requireAdmin(setupOpen, s.handleSaveS3))
	m.HandleFunc("POST /console/smtp", s.requireAdmin(setupOpen, s.handleSaveSMTP))
	m.HandleFunc("POST /console/auth", s.requireAdmin(setupOpen, s.handleSaveAuth))
	m.HandleFunc("POST /console/dirperm", s.requireAdmin(setupOpen, s.handleSaveDirPerm))
	m.HandleFunc("POST /console/notice", s.requireAdmin(setupOpen, s.handleSaveNotice))
	m.HandleFunc("POST /console/syslog", s.requireAdmin(setupOpen, s.handleSaveSyslog))
	m.HandleFunc("POST /api/console/test-s3", s.requireAdminAPI(setupOpen, s.handleTestS3))
	m.HandleFunc("POST /api/console/test-smtp", s.requireAdminAPI(setupOpen, s.handleTestSMTP))
	m.HandleFunc("POST /api/console/test-syslog", s.requireAdminAPI(setupOpen, s.handleTestSyslog))

	// 审计日志展示的是既有的用户活动记录（邮箱、文件路径），不属于首次配置
	// 所需，因此不对初始化模式开放。
	m.HandleFunc("GET /console/audit", s.requireAdmin(adminStrict, s.handleAuditPage))
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

// 管理员端点对初始化模式（尚未配置任何管理员）的策略，在注册路由处显式声明。
const (
	// setupOpen：初始化模式下放行。仅用于控制台自身的首次配置——
	// 否则在配置出第一个管理员之前无人能完成初始化。
	setupOpen = true
	// adminStrict：必须是确已登录的管理员，初始化模式下同样拒绝。
	// 控制台之外的管理员功能都应使用它，避免初始化窗口期被匿名调用。
	adminStrict = false
)

// isAdminReq 报告请求是否来自管理员。
// allowSetup 见 setupOpen / adminStrict 的说明。
func (s *Server) isAdminReq(r *http.Request, allowSetup bool) bool {
	cfg := s.cfg.Get()
	if allowSetup && cfg.SetupMode() {
		return true
	}
	email := s.currentUser(r)
	return email != "" && cfg.IsAdmin(email)
}

// requesterDesc 描述请求者的身份状态，仅用于服务端日志。
//
// currentUser 把「没带 Cookie」「令牌伪造或过期」「已被移出允许名单」
// 一律折叠成空字符串，但三者的安全含义完全不同：携带无效令牌通常意味着
// 有人在伪造会话，值得单独关注，不该淹没在普通的未登录访问里。
func (s *Server) requesterDesc(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "未登录（无会话 Cookie）"
	}
	email, ok := auth.ParseToken(s.cfg.Secret(), c.Value)
	if !ok {
		return "会话令牌无效或已过期"
	}
	if !s.cfg.Get().IsAllowed(email) {
		return "会话有效但已不在允许名单: " + email
	}
	return email
}

// denyAdmin 以与未知路由完全一致的 404 拒绝请求。
//
// 管理员功能对非管理员应当"不存在"而非"无权限"：403 会暴露该端点的存在，
// 让普通用户看到自己用不到的功能、也给探测者提供了可枚举的目标。
// 所有管理员端点都经由本函数拒绝，保证行为一致。
func (s *Server) denyAdmin(w http.ResponseWriter, r *http.Request) {
	log.Printf("拒绝非管理员访问 %s %q（来自 %s，身份: %s）",
		r.Method, r.URL.Path, clientIP(r), s.requesterDesc(r))
	http.NotFound(w, r)
}

// requireAdmin 页面版：已登录的非管理员按不存在处理；
// 完全未登录则走正常登录流程（与其他需要登录的页面一致）。
func (s *Server) requireAdmin(allowSetup bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.isAdminReq(r, allowSetup) {
			next(w, r)
			return
		}
		if s.currentUser(r) == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		s.denyAdmin(w, r)
	}
}

// requireAdminAPI 接口版：任何非管理员（含未登录）一律按不存在处理。
// 接口不做登录跳转，用 401 区分“未登录”同样会暴露端点存在。
func (s *Server) requireAdminAPI(allowSetup bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdminReq(r, allowSetup) {
			s.denyAdmin(w, r)
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
