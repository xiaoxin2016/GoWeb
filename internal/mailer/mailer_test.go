package mailer

import (
	"bufio"
	"encoding/base64"
	"net"
	"strings"
	"testing"

	"github.com/xiaoxin2016/goweb/internal/config"
)

// fakeSMTP 实现一个只讲明文 SMTP 的最小服务器，记录认证信息与收到的邮件。
type fakeSMTP struct {
	ln        net.Listener
	authMechs string // EHLO 时通告的 AUTH 机制，如 "PLAIN LOGIN"
	gotAuth   string // 收到的认证凭据（解码后 user:pass）
	gotData   string
}

func newFakeSMTP(t *testing.T, authMechs string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln, authMechs: authMechs}
	t.Cleanup(func() { ln.Close() })
	go s.serve(t)
	return s
}

func (s *fakeSMTP) addr() (host string, port int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (s *fakeSMTP) serve(t *testing.T) {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(line string) { conn.Write([]byte(line + "\r\n")) }

	w("220 fake.local ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			w("250-fake.local")
			if s.authMechs != "" {
				w("250-AUTH " + s.authMechs)
			}
			w("250 8BITMIME")
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 {
				s.gotAuth = parts[1] + ":" + parts[2]
			}
			w("235 ok")
		case strings.HasPrefix(cmd, "AUTH LOGIN"):
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
			u, _ := r.ReadString('\n')
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
			p, _ := r.ReadString('\n')
			ub, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(u))
			pb, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(p))
			s.gotAuth = string(ub) + ":" + string(pb)
			w("235 ok")
		case strings.HasPrefix(cmd, "MAIL FROM"), strings.HasPrefix(cmd, "RCPT TO"):
			w("250 ok")
		case cmd == "DATA":
			w("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				b.WriteString(l)
			}
			s.gotData = b.String()
			w("250 queued")
		case cmd == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

// TestSendPlainOverUnencrypted 复现并验证修复：25 端口不加密 + PLAIN 认证。
// （标准库 smtp.PlainAuth 会直接报 "unencrypted connection" 拒绝发送。）
func TestSendPlainOverUnencrypted(t *testing.T) {
	srv := newFakeSMTP(t, "PLAIN LOGIN")
	host, port := srv.addr()

	err := Send(config.SMTPConfig{
		Host: host, Port: port,
		Username: "user@corp.local", Password: "secret",
		From: "noreply@corp.local", Encryption: "none",
	}, "to@corp.local", "测试主题", "测试正文")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if srv.gotAuth != "user@corp.local:secret" {
		t.Errorf("认证凭据 = %q", srv.gotAuth)
	}
	if !strings.Contains(srv.gotData, "To: to@corp.local") {
		t.Errorf("邮件内容缺少收件人头: %q", srv.gotData)
	}
}

// TestSendLoginOnlyServer 验证仅支持 LOGIN 机制的旧服务器。
func TestSendLoginOnlyServer(t *testing.T) {
	srv := newFakeSMTP(t, "LOGIN")
	host, port := srv.addr()

	err := Send(config.SMTPConfig{
		Host: host, Port: port,
		Username: "olduser", Password: "oldpass",
		From: "noreply@corp.local", Encryption: "none",
	}, "to@corp.local", "subject", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if srv.gotAuth != "olduser:oldpass" {
		t.Errorf("认证凭据 = %q", srv.gotAuth)
	}
}

// TestSendNoAuthServer 验证不支持 AUTH 的内部中继：跳过认证直接投递。
func TestSendNoAuthServer(t *testing.T) {
	srv := newFakeSMTP(t, "")
	host, port := srv.addr()

	err := Send(config.SMTPConfig{
		Host: host, Port: port,
		Username: "ignored", Password: "ignored",
		From: "noreply@corp.local", Encryption: "none",
	}, "to@corp.local", "subject", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if srv.gotAuth != "" {
		t.Errorf("不应发送认证, gotAuth = %q", srv.gotAuth)
	}
	if srv.gotData == "" {
		t.Error("邮件未投递")
	}
}
