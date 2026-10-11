package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
)

// 会话。
//
// 会话以 map 的形式保存在内存里：Cookie 中只有一串随机令牌，服务端据此查出邮箱。
// 与无状态的签名令牌相比，退出登录会真正作废令牌（签名令牌在到期前始终有效，
// 只能清掉浏览器里的 Cookie）。代价是进程重启会丢失会话，因此进程优雅退出
// （SIGTERM / SIGINT）时把仍然有效的会话写入快照文件，下次启动读回，
// 升级、灰度发版不会把所有人踢回登录页。
//
// 几条刻意的限制：
//   - 只在优雅退出时保存。kill -9 或崩溃时进程来不及执行任何代码，不会留下快照，
//     重启后所有人需要重新登录——这是预期行为，不是缺陷。
//   - 快照读完即删。否则之后一次 kill -9 会留下旧快照，再启动就把早已退出登录的会话
//     恢复回来；顺带让这份含有效凭据的文件只在“停止到下次启动”之间存在。
//   - 权限或内容不对就整份丢弃，一个会话都不恢复：快照是程序自己写的，出现不合规
//     内容只可能是写到一半被打断或被人改过，两种都不该采信。
//   - 恢复的会话沿用原有效期，重启不续命；有效期超过当前会话时长上限的也视为被篡改。

// sessionTokenBytes 会话令牌的随机字节数；经 base64url 无填充编码后固定 43 个字符。
const sessionTokenBytes = 32

// sessionSweepEvery 两次清理过期会话的最小间隔（在新建会话时顺带进行，无需后台协程）。
const sessionSweepEvery = 5 * time.Minute

type session struct {
	email   string
	expires time.Time
}

// SessionStore 内存中的会话表，并发安全。
type SessionStore struct {
	mu        sync.Mutex
	m         map[string]session
	lastSweep time.Time
}

// NewSessionStore 创建空的会话表。
func NewSessionStore() *SessionStore {
	return &SessionStore{m: map[string]session{}}
}

// Create 为邮箱新建一个会话，返回令牌与到期时间。
func (s *SessionStore) Create(email string, ttl time.Duration) (string, time.Time, error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, fmt.Errorf("生成会话令牌: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	now := time.Now()
	expires := now.Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.lastSweep) >= sessionSweepEvery {
		s.sweepLocked(now)
	}
	s.m[token] = session{email: normalize(email), expires: expires}
	return token, expires, nil
}

// Lookup 返回令牌对应的邮箱；令牌不存在或已过期时返回 false（过期的顺带删除）。
func (s *SessionStore) Lookup(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.m[token]
	if !ok {
		return "", false
	}
	if time.Now().After(ss.expires) {
		delete(s.m, token)
		return "", false
	}
	return ss.email, true
}

// Delete 作废一个会话（退出登录）。
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// Len 返回当前会话数（含尚未清理的过期会话）。
func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func (s *SessionStore) sweepLocked(now time.Time) {
	for token, ss := range s.m {
		if now.After(ss.expires) {
			delete(s.m, token)
		}
	}
	s.lastSweep = now
}

// ---- 快照 ----

// sessionSnapshot 快照的落盘格式。令牌等同于凭据本身，文件只以 0600 写出。
type sessionSnapshot struct {
	SavedAt  time.Time         `json:"saved_at"`
	Sessions []sessionSnapItem `json:"sessions"`
}

type sessionSnapItem struct {
	Token   string `json:"token"`
	Email   string `json:"email"`
	Expires int64  `json:"expires"` // Unix 秒
}

// Save 把仍然有效的会话写入快照文件，返回保存的会话数；没有有效会话时不留文件。
// 应在进程即将退出、已停止接收新请求之后调用。
func (s *SessionStore) Save(path string) (int, error) {
	now := time.Now()
	snap := sessionSnapshot{SavedAt: now}
	s.mu.Lock()
	for token, ss := range s.m {
		if now.After(ss.expires) {
			continue
		}
		snap.Sessions = append(snap.Sessions, sessionSnapItem{Token: token, Email: ss.email, Expires: ss.expires.Unix()})
	}
	s.mu.Unlock()

	// 先删再写：os.WriteFile 不会修正已存在文件的权限位
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if len(snap.Sessions) == 0 {
		return 0, nil
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return 0, err
	}
	return len(snap.Sessions), nil
}

// RestoreResult 恢复快照的结果，供调用方记录日志。
type RestoreResult struct {
	Found    bool      // 是否存在快照文件
	Restored int       // 恢复的会话数
	Expired  int       // 已过期而跳过的会话数
	SavedAt  time.Time // 快照保存时间
}

// Restore 读回快照并立即删除快照文件。maxTTL 为当前的会话时长上限，
// validEmail 校验邮箱格式。任何不合规都整份丢弃并返回错误（此时不恢复任何会话）。
func (s *SessionStore) Restore(path string, maxTTL time.Duration, validEmail func(string) bool) (RestoreResult, error) {
	var res RestoreResult
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil // 首次启动、上次未优雅退出：都是常态
	}
	if err != nil {
		return res, err
	}
	res.Found = true
	// 读完即删：无论下面解析成功与否，这份快照都不该留到下一次启动
	defer os.Remove(path)

	if !fi.Mode().IsRegular() {
		return res, errors.New("快照不是普通文件")
	}
	// 文件里是等同于密码的令牌：别人能写它，就等于能给自己造一个登录态
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return res, fmt.Errorf("快照权限为 %04o（应为 0600）", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return res, err
	}
	var snap sessionSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return res, errors.New("快照格式不正确")
	}
	res.SavedAt = snap.SavedAt

	now := time.Now()
	restored := map[string]session{}
	for _, it := range snap.Sessions {
		if !validSessionToken(it.Token) || !validEmail(it.Email) {
			return RestoreResult{Found: true, SavedAt: snap.SavedAt}, errors.New("快照内容不合规")
		}
		exp := time.Unix(it.Expires, 0)
		// 被改过的文件不能靠改有效期给自己换一个长期有效的身份
		if exp.After(now.Add(maxTTL)) {
			return RestoreResult{Found: true, SavedAt: snap.SavedAt}, errors.New("快照中的有效期超出当前会话时长上限")
		}
		if now.After(exp) {
			res.Expired++
			continue
		}
		restored[it.Token] = session{email: normalize(it.Email), expires: exp}
	}

	s.mu.Lock()
	for token, ss := range restored {
		s.m[token] = ss
	}
	s.mu.Unlock()
	res.Restored = len(restored)
	return res, nil
}

// validSessionToken 校验令牌格式：固定长度的 base64url 字符串，
// 免得把任意内容当成 map 的键塞进来。
func validSessionToken(t string) bool {
	if len(t) != base64.RawURLEncoding.EncodedLen(sessionTokenBytes) {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
