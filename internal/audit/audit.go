// Package audit 记录用户操作审计事件：本地 JSONL 文件持久化、
// 内存环形缓冲供控制台查询，并可选通过 syslog 协议（RFC 3164）
// 外发到 rsyslog 等日志服务器。
package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/xiaoxin2016/goweb/internal/config"
)

// Event 一条审计事件。
type Event struct {
	Time   time.Time `json:"time"`
	User   string    `json:"user"`
	Action string    `json:"action"` // download | upload | delete
	Path   string    `json:"path"`   // 操作对象（目录或文件的相对路径）
	IP     string    `json:"ip"`
	Result string    `json:"result"` // "ok" 或错误描述
}

const (
	ringSize   = 500             // 内存中保留的最近事件数
	tailBytes  = 256 * 1024      // 启动时从审计文件尾部加载的最大字节数
	sysQueue   = 1000            // syslog 异步发送队列长度
	sendTimout = 5 * time.Second // syslog 连接/写超时
)

// Logger 审计日志记录器，方法并发安全。
type Logger struct {
	mu   sync.Mutex
	file *os.File
	ring []Event

	sysMu  sync.Mutex
	sysCfg config.SyslogConfig
	sysCh  chan string
}

// New 打开（或创建）数据目录下的 audit.log，并加载最近事件到内存。
func New(dataDir string) (*Logger, error) {
	path := filepath.Join(dataDir, "audit.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志文件: %w", err)
	}
	l := &Logger{file: f, sysCh: make(chan string, sysQueue)}
	l.loadTail(path)
	go l.syslogWorker()
	return l, nil
}

// Configure 应用 syslog 外发配置（可随时热更新）。
func (l *Logger) Configure(cfg config.SyslogConfig) {
	l.sysMu.Lock()
	defer l.sysMu.Unlock()
	l.sysCfg = cfg
}

// Log 记录一条事件：写文件、入环形缓冲，并（若开启）异步外发 syslog。
func (l *Logger) Log(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Result == "" {
		e.Result = "ok"
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}

	l.mu.Lock()
	l.file.Write(append(line, '\n'))
	l.ring = append(l.ring, e)
	if len(l.ring) > ringSize {
		l.ring = l.ring[len(l.ring)-ringSize:]
	}
	l.mu.Unlock()

	l.sysMu.Lock()
	enabled := l.sysCfg.Enabled
	l.sysMu.Unlock()
	if enabled {
		select {
		case l.sysCh <- string(line):
		default: // 队列满则丢弃，绝不阻塞业务请求
		}
	}
}

// Recent 返回最近的事件（新→旧），可按操作类型与用户过滤。
func (l *Logger) Recent(n int, action, user string) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, 0, n)
	for i := len(l.ring) - 1; i >= 0 && len(out) < n; i-- {
		e := l.ring[i]
		if action != "" && e.Action != action {
			continue
		}
		if user != "" && e.User != user {
			continue
		}
		out = append(out, e)
	}
	return out
}

// loadTail 从审计文件尾部恢复最近事件到环形缓冲。
func (l *Logger) loadTail(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	offset := int64(0)
	if st.Size() > tailBytes {
		offset = st.Size() - tailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return
	}
	lines := bytes.Split(raw, []byte{'\n'})
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:] // 丢弃可能被截断的首行
	}
	for _, ln := range lines {
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		var e Event
		if json.Unmarshal(ln, &e) == nil {
			l.ring = append(l.ring, e)
		}
	}
	if len(l.ring) > ringSize {
		l.ring = l.ring[len(l.ring)-ringSize:]
	}
}

// ---- syslog 外发（RFC 3164，自实现以保证跨平台与写超时控制）----

func (l *Logger) syslogWorker() {
	var conn net.Conn
	closeConn := func() {
		if conn != nil {
			conn.Close()
			conn = nil
		}
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "goweb"
	}

	for msg := range l.sysCh {
		l.sysMu.Lock()
		cfg := l.sysCfg
		l.sysMu.Unlock()
		if !cfg.Enabled || cfg.Address == "" {
			closeConn()
			continue
		}
		// 最多尝试两次（第一次失败后重连再发一次）
		for attempt := 0; attempt < 2; attempt++ {
			if conn == nil {
				c, err := dialSyslog(cfg)
				if err != nil {
					break // 目标不可达，丢弃本条，等下一条再试
				}
				conn = c
			}
			if err := writeSyslog(conn, cfg, hostname, msg); err != nil {
				closeConn()
				continue
			}
			break
		}
	}
	closeConn()
}

func dialSyslog(cfg config.SyslogConfig) (net.Conn, error) {
	network := cfg.Network
	if network != "tcp" {
		network = "udp"
	}
	return net.DialTimeout(network, cfg.Address, sendTimout)
}

func writeSyslog(conn net.Conn, cfg config.SyslogConfig, hostname, msg string) error {
	pri := cfg.FacilityNum()*8 + 6 // severity 6 = info
	tag := cfg.Tag
	if tag == "" {
		tag = "goweb-audit"
	}
	line := fmt.Sprintf("<%d>%s %s %s: %s",
		pri, time.Now().Format(time.Stamp), hostname, tag, msg)
	if _, ok := conn.(*net.TCPConn); ok {
		line += "\n" // TCP 传输以换行分帧
	}
	conn.SetWriteDeadline(time.Now().Add(sendTimout))
	_, err := conn.Write([]byte(line))
	return err
}

// SendTest 使用给定配置同步发送一条测试消息（供控制台“测试”按钮使用）。
func SendTest(cfg config.SyslogConfig) error {
	if cfg.Address == "" {
		return fmt.Errorf("请填写 rsyslog 服务器地址（host:port）")
	}
	conn, err := dialSyslog(cfg)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", cfg.Address, err)
	}
	defer conn.Close()
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "goweb"
	}
	msg := fmt.Sprintf(`{"time":%q,"action":"test","result":"ok"}`,
		time.Now().Format(time.RFC3339))
	if err := writeSyslog(conn, cfg, hostname, msg); err != nil {
		return fmt.Errorf("发送失败: %w", err)
	}
	return nil
}
