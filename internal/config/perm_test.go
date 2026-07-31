package config

import "testing"

func permCfg() Config {
	return Config{
		Auth: AuthConfig{
			AdminEmails:  []string{"admin@test.com"},
			ReadOnlyDirs: []string{"public", "archive"},
		},
	}
}

func TestTopDir(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"a.txt":          "", // 根目录下的文件不属于任何一级目录
		"docs/":          "docs",
		"docs/a.txt":     "docs",
		"docs/sub/b.txt": "docs",
		"docs/sub/":      "docs",
		"/docs/a.txt":    "docs",
	}
	for in, want := range cases {
		if got := TopDir(in); got != want {
			t.Errorf("TopDir(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestCanWrite(t *testing.T) {
	c := permCfg()
	const user, admin = "user@test.com", "admin@test.com"

	cases := []struct {
		email, path string
		want        bool
	}{
		// 普通用户：只读目录内的任何层级都不可写
		{user, "public/", false},
		{user, "public/a.txt", false},
		{user, "public/sub/deep/c.bin", false},
		{user, "archive/x/", false},
		// 普通用户：非只读目录与根级文件可写
		{user, "shared/a.txt", true},
		{user, "shared/", true},
		{user, "root-file.txt", true},
		// 管理员不受限制
		{admin, "public/a.txt", true},
		{admin, "public/", true},
		{admin, "archive/x/y.txt", true},
	}
	for _, tc := range cases {
		if got := c.CanWrite(tc.email, tc.path); got != tc.want {
			t.Errorf("CanWrite(%q, %q) = %v, 期望 %v", tc.email, tc.path, got, tc.want)
		}
	}
}

func TestIsReadOnlyDir(t *testing.T) {
	c := permCfg()
	// 配置中带斜杠的写法也应能匹配
	c.Auth.ReadOnlyDirs = append(c.Auth.ReadOnlyDirs, "/slashed/")

	for _, name := range []string{"public", "archive", "slashed"} {
		if !c.IsReadOnlyDir(name) {
			t.Errorf("IsReadOnlyDir(%q) = false, 期望 true", name)
		}
	}
	for _, name := range []string{"", "shared", "publicx", "pub"} {
		if c.IsReadOnlyDir(name) {
			t.Errorf("IsReadOnlyDir(%q) = true, 期望 false", name)
		}
	}
}
