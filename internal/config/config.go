// Package config 负责应用配置的加载、保存与并发安全访问。
// 配置以 JSON 文件形式持久化在数据目录中，可通过 Web 控制台在线修改。
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// S3Config 对象存储（S3 / OSS / MinIO 等 S3 兼容服务）配置。
type S3Config struct {
	Endpoint   string `json:"endpoint"`    // 例如 https://oss-cn-hangzhou.aliyuncs.com 或 http://127.0.0.1:9000
	Region     string `json:"region"`      // 区域，留空默认 us-east-1
	AccessKey  string `json:"access_key"`  // AK
	SecretKey  string `json:"secret_key"`  // SK
	Bucket     string `json:"bucket"`      // 存储桶名称
	RootPrefix string `json:"root_prefix"` // 仅浏览该前缀（文件夹）下的内容，留空为整个桶
	PathStyle  bool   `json:"path_style"`  // MinIO 等自建服务通常需要 Path-Style 寻址
	// InsecureTLS 跳过 HTTPS 证书校验，用于私有云自签名证书场景。
	// 开启后无法防御中间人攻击，仅在可信内网中使用。
	InsecureTLS bool `json:"insecure_tls"`
}

// Ready 返回 S3 配置是否已具备可用的最小字段。
func (s S3Config) Ready() bool {
	return s.Bucket != "" && s.AccessKey != "" && s.SecretKey != ""
}

// NormalizedRoot 返回规范化的根前缀：非空时保证以 "/" 结尾且不以 "/" 开头。
func (s S3Config) NormalizedRoot() string {
	p := strings.Trim(s.RootPrefix, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// SMTPConfig 邮件发送配置，用于发送登录验证码。
type SMTPConfig struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	From       string `json:"from"`       // 发件人地址，形如 noreply@example.com
	Encryption string `json:"encryption"` // none | ssl | starttls
	// InsecureTLS 跳过 SSL/STARTTLS 的证书校验，用于内网自签名证书的邮件服务器。
	InsecureTLS bool `json:"insecure_tls"`
}

// Ready 返回 SMTP 配置是否已具备可用的最小字段。
func (s SMTPConfig) Ready() bool {
	return s.Host != "" && s.Port > 0 && s.From != ""
}

// AuthConfig 登录与权限配置。
type AuthConfig struct {
	// AdminEmails 管理员邮箱，可访问控制台。为空时系统处于“初始化模式”。
	AdminEmails []string `json:"admin_emails"`
	// AllowedEmails 允许登录的邮箱，支持精确匹配与 *@domain.com 通配。
	// 管理员邮箱始终允许登录。
	AllowedEmails []string `json:"allowed_emails"`
	// SessionHours 登录会话有效期（小时），默认 168（7 天）。
	SessionHours int `json:"session_hours"`
}

// SyslogConfig 审计日志通过 syslog 协议外发（对接 rsyslog）的配置。
type SyslogConfig struct {
	Enabled  bool   `json:"enabled"`
	Network  string `json:"network"`  // udp | tcp
	Address  string `json:"address"`  // host:port，rsyslog 默认 514
	Tag      string `json:"tag"`      // syslog 标签，默认 goweb-audit
	Facility string `json:"facility"` // local0 ~ local7，默认 local0
}

// FacilityNum 返回 facility 的数值（local0=16 … local7=23）。
func (s SyslogConfig) FacilityNum() int {
	if len(s.Facility) == 6 && strings.HasPrefix(s.Facility, "local") {
		if d := s.Facility[5]; d >= '0' && d <= '7' {
			return 16 + int(d-'0')
		}
	}
	return 16 // local0
}

// Config 应用完整配置。
type Config struct {
	Title  string       `json:"title"`
	S3     S3Config     `json:"s3"`
	SMTP   SMTPConfig   `json:"smtp"`
	Auth   AuthConfig   `json:"auth"`
	Syslog SyslogConfig `json:"syslog"`
}

// SetupMode 报告系统是否尚未完成初始化（未配置任何管理员）。
func (c Config) SetupMode() bool { return len(c.Auth.AdminEmails) == 0 }

// IsAdmin 报告邮箱是否为管理员。
func (c Config) IsAdmin(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, a := range c.Auth.AdminEmails {
		if strings.ToLower(strings.TrimSpace(a)) == email {
			return true
		}
	}
	return false
}

// IsAllowed 报告邮箱是否允许登录（管理员或在允许列表中）。
func (c Config) IsAllowed(email string) bool {
	if c.IsAdmin(email) {
		return true
	}
	email = strings.ToLower(strings.TrimSpace(email))
	for _, pat := range c.Auth.AllowedEmails {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == "" {
			continue
		}
		if strings.HasPrefix(pat, "*@") {
			if strings.HasSuffix(email, pat[1:]) {
				return true
			}
		} else if pat == email {
			return true
		}
	}
	return false
}

// SessionDurationHours 返回会话有效期，未配置时为默认值。
func (c Config) SessionDurationHours() int {
	if c.Auth.SessionHours > 0 {
		return c.Auth.SessionHours
	}
	return 168
}

// Store 提供配置的并发安全读写与持久化。
type Store struct {
	mu      sync.RWMutex
	path    string
	dataDir string
	cfg     Config
	secret  []byte
	rev     int64 // 每次保存自增，用于让缓存的 S3 客户端失效
}

// DataDir 返回数据目录路径。
func (s *Store) DataDir() string { return s.dataDir }

// Load 从数据目录加载配置；目录或文件不存在时会自动创建。
// 同时加载（或生成）用于签发会话令牌的密钥。
func Load(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	s := &Store{path: filepath.Join(dataDir, "config.json"), dataDir: dataDir}
	s.cfg.Title = "GoWeb 文件浏览"

	if raw, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(raw, &s.cfg); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", s.path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	secretPath := filepath.Join(dataDir, "secret")
	if raw, err := os.ReadFile(secretPath); err == nil && len(raw) >= 32 {
		s.secret = raw
	} else {
		s.secret = make([]byte, 32)
		if _, err := rand.Read(s.secret); err != nil {
			return nil, err
		}
		if err := os.WriteFile(secretPath, s.secret, 0o600); err != nil {
			return nil, fmt.Errorf("写入密钥文件: %w", err)
		}
	}
	return s, nil
}

// Get 返回当前配置的副本。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Secret 返回会话签名密钥。
func (s *Store) Secret() []byte { return s.secret }

// Rev 返回配置修订号。
func (s *Store) Rev() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rev
}

// Update 在锁内应用修改并持久化到磁盘。
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	s.rev++
	raw, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
