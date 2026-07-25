package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xiaoxin2016/goweb/internal/audit"
	"github.com/xiaoxin2016/goweb/internal/config"
	"github.com/xiaoxin2016/goweb/internal/mailer"
	"github.com/xiaoxin2016/goweb/internal/storage"
)

type consoleData struct {
	Title     string
	Cfg       config.Config
	SetupMode bool
	User      string
	Msg       string
	Err       string
	// 密钥类字段不回显原文，仅提示是否已配置
	HasS3Secret   bool
	HasSMTPSecret bool
	AdminsText    string
	AllowedText   string
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	s.render(w, "console.html", consoleData{
		Title:         cfg.Title,
		Cfg:           cfg,
		SetupMode:     cfg.SetupMode(),
		User:          s.currentUser(r),
		Msg:           r.URL.Query().Get("msg"),
		Err:           r.URL.Query().Get("err"),
		HasS3Secret:   cfg.S3.SecretKey != "",
		HasSMTPSecret: cfg.SMTP.Password != "",
		AdminsText:    strings.Join(cfg.Auth.AdminEmails, "\n"),
		AllowedText:   strings.Join(cfg.Auth.AllowedEmails, "\n"),
	})
}

func redirectConsole(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	http.Redirect(w, r, "/console?"+q.Encode(), http.StatusSeeOther)
}

// handleSaveS3 保存对象存储配置。密钥留空表示保持不变。
func (s *Server) handleSaveS3(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectConsole(w, r, "", "表单解析失败")
		return
	}
	err := s.cfg.Update(func(c *config.Config) {
		c.S3.Endpoint = strings.TrimSpace(r.FormValue("endpoint"))
		c.S3.Region = strings.TrimSpace(r.FormValue("region"))
		c.S3.AccessKey = strings.TrimSpace(r.FormValue("access_key"))
		if sk := strings.TrimSpace(r.FormValue("secret_key")); sk != "" {
			c.S3.SecretKey = sk
		}
		c.S3.Bucket = strings.TrimSpace(r.FormValue("bucket"))
		c.S3.RootPrefix = strings.TrimSpace(r.FormValue("root_prefix"))
		c.S3.PathStyle = r.FormValue("path_style") == "on"
		c.S3.InsecureTLS = r.FormValue("insecure_tls") == "on"
		if t := strings.TrimSpace(r.FormValue("title")); t != "" {
			c.Title = t
		}
	})
	if err != nil {
		redirectConsole(w, r, "", "保存失败: "+err.Error())
		return
	}
	redirectConsole(w, r, "对象存储配置已保存", "")
}

// handleSaveSMTP 保存邮件配置。密码留空表示保持不变。
func (s *Server) handleSaveSMTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectConsole(w, r, "", "表单解析失败")
		return
	}
	port, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	err := s.cfg.Update(func(c *config.Config) {
		c.SMTP.Host = strings.TrimSpace(r.FormValue("host"))
		c.SMTP.Port = port
		c.SMTP.Username = strings.TrimSpace(r.FormValue("username"))
		if pw := r.FormValue("password"); pw != "" {
			c.SMTP.Password = pw
		}
		c.SMTP.From = strings.TrimSpace(r.FormValue("from"))
		c.SMTP.Encryption = r.FormValue("encryption")
		c.SMTP.InsecureTLS = r.FormValue("insecure_tls") == "on"
	})
	if err != nil {
		redirectConsole(w, r, "", "保存失败: "+err.Error())
		return
	}
	redirectConsole(w, r, "邮件配置已保存", "")
}

// handleSaveAuth 保存登录权限配置。
func (s *Server) handleSaveAuth(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectConsole(w, r, "", "表单解析失败")
		return
	}
	admins := splitLines(r.FormValue("admin_emails"))
	allowed := splitLines(r.FormValue("allowed_emails"))
	for _, e := range admins {
		if _, err := mail.ParseAddress(e); err != nil {
			redirectConsole(w, r, "", fmt.Sprintf("管理员邮箱 %q 格式不正确", e))
			return
		}
	}
	for _, e := range allowed {
		if !strings.HasPrefix(e, "*@") {
			if _, err := mail.ParseAddress(e); err != nil {
				redirectConsole(w, r, "", fmt.Sprintf("邮箱 %q 格式不正确（通配请使用 *@example.com）", e))
				return
			}
		}
	}
	// 防呆：初始化完成后不允许把管理员清空，否则系统会退回无鉴权的初始化模式
	if len(admins) == 0 && !s.cfg.Get().SetupMode() {
		redirectConsole(w, r, "", "至少需要保留一个管理员邮箱")
		return
	}
	hours, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("session_hours")))
	err := s.cfg.Update(func(c *config.Config) {
		c.Auth.AdminEmails = admins
		c.Auth.AllowedEmails = allowed
		c.Auth.SessionHours = hours
	})
	if err != nil {
		redirectConsole(w, r, "", "保存失败: "+err.Error())
		return
	}
	redirectConsole(w, r, "登录权限配置已保存", "")
}

func splitLines(v string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(v, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';' || r == ' '
	}) {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// noticeMaxLen 公告文本长度上限，避免条带撑爆页面。
const noticeMaxLen = 500

// handleSaveNotice 保存公告栏配置。
func (s *Server) handleSaveNotice(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectConsole(w, r, "", "表单解析失败")
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if utf8.RuneCountInString(text) > noticeMaxLen {
		redirectConsole(w, r, "", fmt.Sprintf("公告内容不能超过 %d 个字", noticeMaxLen))
		return
	}
	enabled := r.FormValue("enabled") == "on"
	if enabled && text == "" {
		redirectConsole(w, r, "", "启用公告时必须填写公告内容")
		return
	}
	err := s.cfg.Update(func(c *config.Config) {
		c.Notice.Enabled = enabled
		c.Notice.Text = text
		c.Notice.Level = r.FormValue("level")
		c.Notice.Dismissible = r.FormValue("dismissible") == "on"
	})
	if err != nil {
		redirectConsole(w, r, "", "保存失败: "+err.Error())
		return
	}
	redirectConsole(w, r, "公告栏配置已保存", "")
}

// handleSaveSyslog 保存审计日志外发（rsyslog）配置并立即生效。
func (s *Server) handleSaveSyslog(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectConsole(w, r, "", "表单解析失败")
		return
	}
	enabled := r.FormValue("enabled") == "on"
	address := strings.TrimSpace(r.FormValue("address"))
	if enabled && address == "" {
		redirectConsole(w, r, "", "开启外发时必须填写 rsyslog 服务器地址")
		return
	}
	err := s.cfg.Update(func(c *config.Config) {
		c.Syslog.Enabled = enabled
		c.Syslog.Network = r.FormValue("network")
		c.Syslog.Address = address
		c.Syslog.Tag = strings.TrimSpace(r.FormValue("tag"))
		c.Syslog.Facility = r.FormValue("facility")
	})
	if err != nil {
		redirectConsole(w, r, "", "保存失败: "+err.Error())
		return
	}
	s.audit.Configure(s.cfg.Get().Syslog)
	redirectConsole(w, r, "审计日志外发配置已保存", "")
}

// handleTestSyslog 用表单中的配置发送一条测试 syslog 消息。
func (s *Server) handleTestSyslog(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Network  string `json:"network"`
		Address  string `json:"address"`
		Tag      string `json:"tag"`
		Facility string `json:"facility"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求参数错误"))
		return
	}
	cfg := config.SyslogConfig{
		Enabled:  true,
		Network:  req.Network,
		Address:  strings.TrimSpace(req.Address),
		Tag:      strings.TrimSpace(req.Tag),
		Facility: req.Facility,
	}
	if err := audit.SendTest(cfg); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "测试消息已发送，请在 rsyslog 服务器上确认接收（UDP 无法感知对端是否收到）",
	})
}

// auditPageData 审计日志页面数据。
type auditPageData struct {
	Title        string
	User         string
	Events       []audit.Event
	FilterAction string
	FilterUser   string
	Actions      []string
}

// handleAuditPage 渲染审计日志查询页面。
func (s *Server) handleAuditPage(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	user := strings.TrimSpace(r.URL.Query().Get("user"))
	s.render(w, "audit.html", auditPageData{
		Title:        s.cfg.Get().Title,
		User:         s.currentUser(r),
		Events:       s.audit.Recent(200, action, user),
		FilterAction: action,
		FilterUser:   user,
		Actions:      []string{"download", "upload", "delete"},
	})
}

// handleTestS3 用表单中的配置（密钥留空则用已保存的）测试对象存储连通性。
func (s *Server) handleTestS3(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint    string `json:"endpoint"`
		Region      string `json:"region"`
		AccessKey   string `json:"access_key"`
		SecretKey   string `json:"secret_key"`
		Bucket      string `json:"bucket"`
		RootPrefix  string `json:"root_prefix"`
		PathStyle   bool   `json:"path_style"`
		InsecureTLS bool   `json:"insecure_tls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求参数错误"))
		return
	}
	cfg := config.S3Config{
		Endpoint:    strings.TrimSpace(req.Endpoint),
		Region:      strings.TrimSpace(req.Region),
		AccessKey:   strings.TrimSpace(req.AccessKey),
		SecretKey:   strings.TrimSpace(req.SecretKey),
		Bucket:      strings.TrimSpace(req.Bucket),
		RootPrefix:  strings.TrimSpace(req.RootPrefix),
		PathStyle:   req.PathStyle,
		InsecureTLS: req.InsecureTLS,
	}
	if cfg.SecretKey == "" {
		cfg.SecretKey = s.cfg.Get().S3.SecretKey
	}
	cli, err := storage.New(cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := opCtx(r)
	defer cancel()
	if err := cli.Test(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("连接失败: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "连接成功，配置可用"})
}

// handleTestSMTP 用表单中的配置发送一封测试邮件。
func (s *Server) handleTestSMTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host        string `json:"host"`
		Port        int    `json:"port"`
		Username    string `json:"username"`
		Password    string `json:"password"`
		From        string `json:"from"`
		Encryption  string `json:"encryption"`
		InsecureTLS bool   `json:"insecure_tls"`
		To          string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求参数错误"))
		return
	}
	to := strings.TrimSpace(req.To)
	if _, err := mail.ParseAddress(to); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请填写有效的测试收件邮箱"))
		return
	}
	cfg := config.SMTPConfig{
		Host:        strings.TrimSpace(req.Host),
		Port:        req.Port,
		Username:    strings.TrimSpace(req.Username),
		Password:    req.Password,
		From:        strings.TrimSpace(req.From),
		Encryption:  req.Encryption,
		InsecureTLS: req.InsecureTLS,
	}
	if cfg.Password == "" {
		cfg.Password = s.cfg.Get().SMTP.Password
	}
	title := s.cfg.Get().Title
	err := mailer.Send(cfg, to, fmt.Sprintf("【%s】SMTP 测试邮件", title),
		"这是一封测试邮件。收到本邮件说明 SMTP 配置正确。")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "测试邮件已发送，请检查收件箱"})
}
