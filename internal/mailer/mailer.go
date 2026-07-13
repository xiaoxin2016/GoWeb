// Package mailer 通过 SMTP 发送邮件，支持 SSL、STARTTLS 与明文三种方式。
package mailer

import (
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/xiaoxin2016/goweb/internal/config"
)

const dialTimeout = 15 * time.Second

// Send 使用给定的 SMTP 配置发送一封纯文本邮件。
func Send(cfg config.SMTPConfig, to, subject, body string) error {
	if !cfg.Ready() {
		return fmt.Errorf("SMTP 尚未配置，请先在控制台完成邮件设置")
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	tlsCfg := &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: cfg.InsecureTLS}

	var (
		client *smtp.Client
		err    error
	)
	switch strings.ToLower(cfg.Encryption) {
	case "ssl", "tls", "smtps":
		conn, dErr := tls.DialWithDialer(&net.Dialer{Timeout: dialTimeout}, "tcp", addr, tlsCfg)
		if dErr != nil {
			return fmt.Errorf("连接 SMTP(SSL) 失败: %w", dErr)
		}
		client, err = smtp.NewClient(conn, cfg.Host)
	default: // none / starttls 先建立明文连接
		conn, dErr := net.DialTimeout("tcp", addr, dialTimeout)
		if dErr != nil {
			return fmt.Errorf("连接 SMTP 失败: %w", dErr)
		}
		client, err = smtp.NewClient(conn, cfg.Host)
	}
	if err != nil {
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	defer client.Close()

	if strings.EqualFold(cfg.Encryption, "starttls") {
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS 失败: %w", err)
		}
	}

	if cfg.Username != "" {
		auth := pickAuth(client, cfg)
		if auth != nil {
			if err := client.Auth(auth); err != nil {
				return fmt.Errorf("SMTP 认证失败: %w", err)
			}
		}
	}

	if err := client.Mail(cfg.From); err != nil {
		return fmt.Errorf("设置发件人失败: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("设置收件人失败: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	msg := buildMessage(cfg.From, to, subject, body)
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// pickAuth 根据服务器通告的机制选择认证方式。
// 标准库的 smtp.PlainAuth 会拒绝在非加密连接上认证；用户在控制台显式选择
// “不加密”即表示接受该风险（常见于仅内网可达的 25 端口服务器），因此这里
// 使用自实现的 PLAIN/LOGIN，不做加密限制。
// 返回 nil 表示服务器不支持 AUTH（如无需认证的内部中继），跳过认证。
func pickAuth(client *smtp.Client, cfg config.SMTPConfig) smtp.Auth {
	ok, mechs := client.Extension("AUTH")
	if !ok {
		return nil
	}
	switch {
	case strings.Contains(mechs, "PLAIN"):
		return plainAuth{username: cfg.Username, password: cfg.Password}
	case strings.Contains(mechs, "LOGIN"):
		return &loginAuth{username: cfg.Username, password: cfg.Password}
	case strings.Contains(mechs, "CRAM-MD5"):
		return smtp.CRAMMD5Auth(cfg.Username, cfg.Password)
	default:
		// 服务器支持 AUTH 但机制不在上述范围，仍尝试 PLAIN
		return plainAuth{username: cfg.Username, password: cfg.Password}
	}
}

// plainAuth 实现 RFC 4616 PLAIN 认证，不限制连接是否加密。
type plainAuth struct {
	username, password string
}

func (a plainAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("PLAIN 认证收到意外的服务器质询")
	}
	return nil, nil
}

// loginAuth 实现 LOGIN 认证（部分旧邮件服务器仅支持此机制）。
type loginAuth struct {
	username, password string
	step               int
}

func (a *loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	a.step = 0
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("LOGIN 认证收到意外的服务器质询")
	}
}

func buildMessage(from, to, subject, body string) string {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("UTF-8", subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	b.WriteString("\r\n")
	return b.String()
}
