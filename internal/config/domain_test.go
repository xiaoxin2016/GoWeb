package config

import "testing"

func TestNormalizeEmail(t *testing.T) {
	withDomain := Config{Auth: AuthConfig{DefaultDomain: "test.com"}}
	noDomain := Config{}

	cases := []struct {
		name string
		cfg  Config
		in   string
		want string
	}{
		{"补全默认域", withDomain, "zhang", "zhang@test.com"},
		{"去空白并转小写", withDomain, "  ZhangSan  ", "zhangsan@test.com"},
		{"已含 @ 不改动", withDomain, "li@other.com", "li@other.com"},
		{"已含 @ 仅转小写", withDomain, "Li@Other.COM", "li@other.com"},
		{"空输入", withDomain, "  ", ""},
		{"未配置默认域时原样返回", noDomain, "zhang", "zhang"},
		{"未配置默认域不影响完整邮箱", noDomain, "a@b.com", "a@b.com"},
	}
	for _, tc := range cases {
		if got := tc.cfg.NormalizeEmail(tc.in); got != tc.want {
			t.Errorf("%s: NormalizeEmail(%q) = %q, 期望 %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// 控制台里带 @ 或大小写混写的域名配置也应生效
func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{
		"test.com":    "test.com",
		"@test.com":   "test.com",
		"  @Test.COM": "test.com",
		"":            "",
		"   ":         "",
	} {
		if got := NormalizeDomain(in); got != want {
			t.Errorf("NormalizeDomain(%q) = %q, 期望 %q", in, got, want)
		}
	}
	c := Config{Auth: AuthConfig{DefaultDomain: "@Test.COM"}}
	if got := c.NormalizeEmail("zhang"); got != "zhang@test.com" {
		t.Errorf("带 @ 的域名配置未生效: %q", got)
	}
}

// 补全后的地址应能正常匹配通配的允许列表与管理员判定
func TestNormalizeEmailWithAllowList(t *testing.T) {
	c := Config{Auth: AuthConfig{
		DefaultDomain: "test.com",
		AdminEmails:   []string{"admin@test.com"},
		AllowedEmails: []string{"*@test.com"},
	}}
	if e := c.NormalizeEmail("admin"); !c.IsAdmin(e) {
		t.Errorf("补全后的 %q 应被判定为管理员", e)
	}
	if e := c.NormalizeEmail("user"); !c.IsAllowed(e) {
		t.Errorf("补全后的 %q 应被允许登录", e)
	}
	if e := c.NormalizeEmail("someone@evil.com"); c.IsAllowed(e) {
		t.Errorf("外域邮箱 %q 不应被允许登录", e)
	}
}
