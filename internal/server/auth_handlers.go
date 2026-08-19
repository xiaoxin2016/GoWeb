package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"os"
	"time"

	"github.com/xiaoxin2016/goweb/internal/auth"
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
	if _, err := mail.ParseAddress(email); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("邮箱格式不正确"))
		return
	}

	if !cfg.IsAllowed(email) {
		// 与发送成功返回一致的提示，避免探测哪些邮箱被允许
		writeJSON(w, http.StatusOK, map[string]string{"message": "如果该邮箱被允许登录，验证码已发送"})
		return
	}

	code, err := s.codes.Issue(email)
	if err != nil {
		writeErr(w, http.StatusTooManyRequests, err)
		return
	}

	body := fmt.Sprintf("您好，\r\n\r\n您的 %s 登录验证码是：%s\r\n\r\n验证码 10 分钟内有效。如果这不是您本人的操作，请忽略本邮件。",
		cfg.Title, code)
	if err := mailer.Send(cfg.SMTP, email, fmt.Sprintf("【%s】登录验证码", cfg.Title), body); err != nil {
		log.Printf("发送验证码到 %s 失败: %v", email, err)
		// 调试模式：邮件发送失败时把验证码打到日志，避免管理员被锁在门外。
		if os.Getenv("GOWEB_DEBUG_CODE") == "1" {
			log.Printf("[调试] %s 的验证码: %s", email, code)
		}
		writeErr(w, http.StatusBadGateway, fmt.Errorf("邮件发送失败，请联系管理员检查 SMTP 配置"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "如果该邮箱被允许登录，验证码已发送"})
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
