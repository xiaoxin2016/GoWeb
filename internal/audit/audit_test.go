package audit

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/xiaoxin2016/goweb/internal/config"
)

func TestLogAndRecent(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Log(Event{User: "a@b.com", Action: "upload", Path: "/x.txt", IP: "1.2.3.4"})
	l.Log(Event{User: "c@d.com", Action: "delete", Path: "/y/", IP: "5.6.7.8", Result: "拒绝"})

	all := l.Recent(10, "", "")
	if len(all) != 2 || all[0].Action != "delete" || all[1].Result != "ok" {
		t.Fatalf("Recent = %+v", all)
	}
	if got := l.Recent(10, "upload", ""); len(got) != 1 || got[0].User != "a@b.com" {
		t.Fatalf("按操作过滤 = %+v", got)
	}
	if got := l.Recent(10, "", "c@d.com"); len(got) != 1 || got[0].Action != "delete" {
		t.Fatalf("按用户过滤 = %+v", got)
	}

	// 重启后应能从文件恢复
	l2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := l2.Recent(10, "", ""); len(got) != 2 {
		t.Fatalf("重启恢复 = %+v", got)
	}
}

// TestSyslogUDP 起一个 UDP 监听验证 syslog 外发格式与内容。
func TestSyslogUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	l, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l.Configure(config.SyslogConfig{
		Enabled: true, Network: "udp",
		Address: pc.LocalAddr().String(),
		Tag:     "goweb-test", Facility: "local3",
	})
	l.Log(Event{User: "a@b.com", Action: "download", Path: "/f.txt", IP: "9.9.9.9"})

	pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("未收到 syslog 消息: %v", err)
	}
	got := string(buf[:n])

	// local3.info = 19*8+6 = 158
	if !strings.HasPrefix(got, "<158>") {
		t.Errorf("PRI 错误: %q", got)
	}
	if !strings.Contains(got, "goweb-test: ") {
		t.Errorf("缺少标签: %q", got)
	}
	// 消息体应为合法 JSON 且包含事件字段
	payload := got[strings.Index(got, "goweb-test: ")+len("goweb-test: "):]
	var e Event
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		t.Fatalf("消息体不是 JSON: %v, %q", err, payload)
	}
	if e.User != "a@b.com" || e.Action != "download" || e.Path != "/f.txt" {
		t.Errorf("事件内容 = %+v", e)
	}
}

func TestSendTest(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if err := SendTest(config.SyslogConfig{
		Enabled: true, Network: "udp", Address: pc.LocalAddr().String(),
	}); err != nil {
		t.Fatal(err)
	}
	pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1024)
	if _, _, err := pc.ReadFrom(buf); err != nil {
		t.Fatalf("未收到测试消息: %v", err)
	}
}
