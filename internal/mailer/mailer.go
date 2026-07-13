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

	var (
		client *smtp.Client
		err    error
	)
	switch strings.ToLower(cfg.Encryption) {
	case "ssl", "tls", "smtps":
		conn, dErr := tls.DialWithDialer(&net.Dialer{Timeout: dialTimeout}, "tcp", addr,
			&tls.Config{ServerName: cfg.Host})
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
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return fmt.Errorf("STARTTLS 失败: %w", err)
		}
	}

	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
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
