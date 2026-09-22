package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/xiaoxin2016/goweb/internal/auth"
	"github.com/xiaoxin2016/goweb/internal/config"
	"github.com/xiaoxin2016/goweb/internal/mailer"
)

type loginData struct {
	Title string
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	if cfg.SetupMode() {
		http.Redirect(w, r, "/console", http.StatusFound)
		return
	}
	if s.currentUser(r) != "" {
		http.Redirect(w, r, "/files/", http.StatusFound)
		return
	}
	s.render(w, "login.html", loginData{Title: cfg.Title})
}

// handleSendCode 向邮箱发送登录验证码。
func (s *Server) handleSendCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求参数错误"))
		return
	}
	// 只填邮箱名时按控制台配置的默认域补全（未配置默认域则原样校验）
	cfg := s.cfg.Get()
	email := cfg.NormalizeEmail(req.Email)
	// 严格校验后才允许进入后续流程：不合法的输入绝不交给邮件投递系统
	if err := config.ValidateEmail(email); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("邮箱格式不正确"))
		return
	}

	// 限流先于允许名单判断，且对两类邮箱一视同仁地登记：
	// 否则只有名单内的邮箱才会触发“发送过于频繁”，攻击者连发两次
	// 即可据此枚举出哪些邮箱可以登录。
	if err := s.codes.Touch(email); err != nil {
		writeErr(w, http.StatusTooManyRequests, err)
		return
	}
	if !cfg.IsAllowed(email) {
		// 与发送成功返回一致的提示，避免探测哪些邮箱被允许
		sentOK(w)
		return
	}

	code, err := s.codes.Issue(email)
	if err != nil {
		writeErr(w, http.StatusTooManyRequests, err)
		return
	}

	// --ignore-email：完全不碰 SMTP，验证码直接打印到控制台
	if s.opts.IgnoreEmail {
		s.codeToConsole("ignore-email", email, code)
		sentOK(w)
		return
	}

	body := fmt.Sprintf("您好，\r\n\r\n您的 %s 登录验证码是：%s\r\n\r\n验证码 10 分钟内有效。如果这不是您本人的操作，请忽略本邮件。",
		cfg.Title, code)
	if err := mailer.Send(cfg.SMTP, email, fmt.Sprintf("【%s】登录验证码", cfg.Title), body); err != nil {
		log.Printf("发送验证码到 %s 失败: %v", email, err)
		// GOWEB_DEBUG_CODE：投递失败时回退到控制台，并让登录流程照常继续。
		// 只把验证码打进日志却仍向前端报错是没有意义的——用户根本走不到
		// 输入验证码那一步，这个兜底也就形同虚设。
		if s.opts.DebugCode {
			s.codeToConsole("debug-code", email, code)
			sentOK(w)
			return
		}
		writeErr(w, http.StatusBadGateway, fmt.Errorf("邮件发送失败，请联系管理员检查 SMTP 配置"))
		return
	}
	sentOK(w)
}

// sentOK 返回发码成功的统一提示。
// 无论真实投递、--ignore-email 还是投递失败回退，响应都必须一致：
// 措辞有别就会泄露邮箱是否在允许名单内，也会暴露 SMTP 的可用状态。
func sentOK(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]string{"message": "如果该邮箱被允许登录，验证码已发送"})
}

// codeToConsole 把验证码打印到服务端控制台，供 --ignore-email 与
// GOWEB_DEBUG_CODE 两条路径共用，保证格式一致、便于检索。
func (s *Server) codeToConsole(mode, email, code string) {
	log.Printf("[%s] %s 的登录验证码: %s（10 分钟内有效）", mode, email, code)
}

// handleVerify 校验验证码并签发会话。
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求参数错误"))
		return
	}
	cfg := s.cfg.Get()
	email := cfg.NormalizeEmail(req.Email)
	if err := config.ValidateEmail(email); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("邮箱格式不正确"))
		return
	}
	if !cfg.IsAllowed(email) {
		writeErr(w, http.StatusForbidden, errors.New("该邮箱不允许登录"))
		return
	}
	if err := s.codes.Verify(email, req.Code); err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}

	ttl := time.Duration(cfg.SessionDurationHours()) * time.Hour
	token := auth.SignToken(s.cfg.Secret(), email, ttl)
	s.setSession(w, r, token, int(ttl.Seconds()))
	log.Printf("用户 %s 登录成功", email)
	writeJSON(w, http.StatusOK, map[string]any{"email": email, "admin": cfg.IsAdmin(email)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.setSession(w, r, "", -1)
	http.Redirect(w, r, "/login", http.StatusFound)
}
