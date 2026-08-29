// Package auth 实现邮箱验证码管理与会话令牌的签发/校验。
// 会话令牌为 HMAC-SHA256 签名的无状态令牌，服务重启后依然有效。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 验证码参数。
const (
	codeTTL      = 10 * time.Minute // 验证码有效期
	resendGap    = time.Minute      // 同一邮箱两次发送的最小间隔
	maxAttempts  = 5                // 单个验证码最大尝试次数
	codeDigits   = 6                // 验证码位数
	maxCodeStore = 10000            // 防止恶意请求撑爆内存
)

var (
	ErrTooFrequent = errors.New("发送过于频繁，请稍后再试")
	ErrCodeInvalid = errors.New("验证码错误或已过期")
)

type codeEntry struct {
	code     string
	expires  time.Time
	attempts int
	lastSent time.Time
}

// CodeManager 管理内存中的邮箱验证码。
type CodeManager struct {
	mu    sync.Mutex
	codes map[string]*codeEntry
	// reqs 记录每个邮箱最近一次“请求验证码”的时间，与 codes 分开存放：
	// 未被允许登录的邮箱也会登记，用于让两类邮箱的限流响应完全一致。
	// 这里不存验证码，避免产生可被空码校验命中的条目。
	reqs map[string]time.Time
}

func NewCodeManager() *CodeManager {
	return &CodeManager{codes: map[string]*codeEntry{}, reqs: map[string]time.Time{}}
}

// Touch 登记一次验证码请求并施加重发间隔限制，不生成验证码。
//
// 无论邮箱是否被允许登录都必须调用：否则只有允许名单内的邮箱才会
// 触发“发送过于频繁”，攻击者连发两次即可据此枚举出哪些邮箱可登录。
func (m *CodeManager) Touch(email string) error {
	email = normalize(email)
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if t, ok := m.reqs[email]; ok && now.Sub(t) < resendGap {
		return ErrTooFrequent
	}
	for e, t := range m.reqs { // 顺带清理过期登记
		if now.Sub(t) >= resendGap {
			delete(m.reqs, e)
		}
	}
	if len(m.reqs) >= maxCodeStore {
		return errors.New("系统繁忙，请稍后再试")
	}
	m.reqs[email] = now
	return nil
}

// Issue 为邮箱生成一个新的验证码。受重发间隔限制。
func (m *CodeManager) Issue(email string) (string, error) {
	email = normalize(email)
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if e, ok := m.codes[email]; ok && now.Sub(e.lastSent) < resendGap {
		return "", ErrTooFrequent
	}
	m.gcLocked(now)
	if len(m.codes) >= maxCodeStore {
		return "", errors.New("系统繁忙，请稍后再试")
	}

	code, err := randomDigits(codeDigits)
	if err != nil {
		return "", err
	}
	m.codes[email] = &codeEntry{code: code, expires: now.Add(codeTTL), lastSent: now}
	return code, nil
}

// Verify 校验验证码，成功后立即作废。
func (m *CodeManager) Verify(email, code string) error {
	email = normalize(email)
	code = strings.TrimSpace(code)
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.codes[email]
	if !ok || time.Now().After(e.expires) {
		return ErrCodeInvalid
	}
	e.attempts++
	if e.attempts > maxAttempts {
		delete(m.codes, email)
		return ErrCodeInvalid
	}
	if subtle.ConstantTimeCompare([]byte(e.code), []byte(code)) != 1 {
		return ErrCodeInvalid
	}
	delete(m.codes, email)
	return nil
}

func (m *CodeManager) gcLocked(now time.Time) {
	for k, e := range m.codes {
		if now.After(e.expires) {
			delete(m.codes, k)
		}
	}
}

func normalize(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func randomDigits(n int) (string, error) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		d, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteByte(byte('0' + d.Int64()))
	}
	return b.String(), nil
}

// SignToken 签发会话令牌，内容为 邮箱|过期时间。
func SignToken(secret []byte, email string, ttl time.Duration) string {
	payload := fmt.Sprintf("%s|%d", normalize(email), time.Now().Add(ttl).Unix())
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ParseToken 校验令牌，返回其中的邮箱地址。
func ParseToken(secret []byte, token string) (string, bool) {
	part := strings.SplitN(token, ".", 2)
	if len(part) != 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(part[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(part[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	fields := strings.SplitN(string(payload), "|", 2)
	if len(fields) != 2 {
		return "", false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return fields[0], true
}
