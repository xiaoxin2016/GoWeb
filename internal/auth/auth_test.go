package auth

import (
	"testing"
	"time"
)

func TestCodeIssueVerify(t *testing.T) {
	m := NewCodeManager()
	code, err := m.Issue("User@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 6 {
		t.Fatalf("验证码长度 = %d", len(code))
	}
	// 错误验证码
	if err := m.Verify("user@example.com", "000000"); err == nil && code != "000000" {
		t.Fatal("错误验证码不应通过")
	}
	// 正确验证码（邮箱大小写不敏感）
	if err := m.Verify("USER@example.com", code); err != nil {
		t.Fatalf("正确验证码应通过: %v", err)
	}
	// 验证码一次性
	if err := m.Verify("user@example.com", code); err == nil {
		t.Fatal("验证码不应可重复使用")
	}
}

func TestCodeResendLimit(t *testing.T) {
	m := NewCodeManager()
	if _, err := m.Issue("a@b.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Issue("a@b.com"); err != ErrTooFrequent {
		t.Fatalf("一分钟内重发应被拒绝, got %v", err)
	}
}

func TestToken(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok := SignToken(secret, "User@Example.com", time.Hour)

	email, ok := ParseToken(secret, tok)
	if !ok || email != "user@example.com" {
		t.Fatalf("ParseToken = %q, %v", email, ok)
	}
	// 篡改令牌
	if _, ok := ParseToken(secret, tok+"x"); ok {
		t.Fatal("篡改后的令牌不应通过")
	}
	// 错误密钥
	if _, ok := ParseToken([]byte("another-secret-another-secret-32"), tok); ok {
		t.Fatal("错误密钥不应通过")
	}
	// 已过期
	expired := SignToken(secret, "a@b.com", -time.Minute)
	if _, ok := ParseToken(secret, expired); ok {
		t.Fatal("过期令牌不应通过")
	}
}
