package config

import "testing"

func TestValidateEmailAccepts(t *testing.T) {
	// 用户明确要求放行的字符：- _ . 以及常规字母数字
	ok := []string{
		"zhangsan@test.com",
		"zhang.san@test.com",
		"zhang_san@test.com",
		"zhang-san@test.com",
		"z.h-a_n9@sub.corp-test.com.cn",
		"a@b.co",
		"USER@Example.COM", // 大小写由归一化处理，校验本身不拒绝
	}
	for _, e := range ok {
		if err := ValidateEmail(e); err != nil {
			t.Errorf("ValidateEmail(%q) 应放行，却报错: %v", e, err)
		}
	}
}

func TestValidateEmailRejects(t *testing.T) {
	bad := map[string]string{
		// 用户点名要拦的特殊字符
		"a?b@test.com":  "问号",
		"a!b@test.com":  "感叹号",
		"a=b@test.com":  "等号",
		"a#b@test.com":  "井号",
		"a&b@test.com":  "与号",
		"a%b@test.com":  "百分号",
		"a+b@test.com":  "加号",
		"a$b@test.com":  "美元符",
		"a'b@test.com":  "单引号",
		"a`b@test.com":  "反引号",
		"a|b@test.com":  "竖线",
		"a;b@test.com":  "分号",
		"a:b@test.com":  "冒号",
		"a*b@test.com":  "星号",
		"a/b@test.com":  "斜杠",
		"a\\b@test.com": "反斜杠",
		// 换行与控制字符（邮件头注入）
		"a@test.com\n":             "尾部换行",
		"a@test.com\r\nBcc: x@y.z": "CRLF 头注入",
		"a\nb@test.com":            "本地部分换行",
		"a@test.com\tx":            "制表符",
		"a@test.com\x00":           "空字符",
		// RFC 5322 允许但不该作为登录标识的形式
		"Bob <bob@test.com>": "显示名形式",
		"<bob@test.com>":     "尖括号",
		`"foo bar"@test.com`: "带引号含空格的本地部分",
		"a b@test.com":       "空格",
		" a@test.com":        "前导空格",
		"a@test.com ":        "尾部空格",
		"a@test.com,b@e.com": "逗号分隔多地址",
		// 结构性问题
		"":              "空串",
		"a@":            "缺域名",
		"@test.com":     "缺本地部分",
		"a@b":           "无点域名",
		"a@[127.0.0.1]": "IP 字面量",
		"a@@test.com":   "多个 @",
		"a@b..c.com":    "域名空标签",
		".a@test.com":   "本地部分以点开头",
		"a.@test.com":   "本地部分以点结尾",
		"a..b@test.com": "连续两个点",
		"-a@test.com":   "以中划线开头",
		"a@-test.com":   "域名段以中划线开头",
		"a@test-.com":   "域名段以中划线结尾",
		"a@test.c":      "顶级域过短",
		"a@test.123":    "顶级域含数字",
		"中文@test.com":   "非 ASCII 本地部分",
		"a@中文.com":      "非 ASCII 域名",
	}
	for e, why := range bad {
		if err := ValidateEmail(e); err == nil {
			t.Errorf("ValidateEmail(%q) 应拒绝（%s），却放行了", e, why)
		}
	}
}

func TestValidateEmailLength(t *testing.T) {
	long := ""
	for len(long) < 65 {
		long += "a"
	}
	if err := ValidateEmail(long + "@test.com"); err == nil {
		t.Error("超长本地部分应被拒绝")
	}
	whole := "a@"
	for len(whole) < 260 {
		whole += "b"
	}
	if err := ValidateEmail(whole + ".com"); err == nil {
		t.Error("超长地址应被拒绝")
	}
}

func TestValidateDomainName(t *testing.T) {
	for _, d := range []string{"test.com", "corp-test.com.cn", "a.co"} {
		if err := ValidateDomainName(d); err != nil {
			t.Errorf("ValidateDomainName(%q) 应放行: %v", d, err)
		}
	}
	for _, d := range []string{"", "test", "test.c", "-test.com", "test-.com",
		"te st.com", "test.com\n", "test..com", "test.com?x", "中文.com"} {
		if err := ValidateDomainName(d); err == nil {
			t.Errorf("ValidateDomainName(%q) 应拒绝", d)
		}
	}
}

// 归一化 + 校验的组合：只填邮箱名时补全后应通过，含非法字符时仍应拒绝
func TestNormalizeThenValidate(t *testing.T) {
	c := Config{Auth: AuthConfig{DefaultDomain: "test.com"}}
	if e := c.NormalizeEmail("zhang_san-1"); ValidateEmail(e) != nil {
		t.Errorf("补全后的 %q 应合法", e)
	}
	for _, in := range []string{"zhang?san", "a b", "x\ny", "a@b@c"} {
		if e := c.NormalizeEmail(in); ValidateEmail(e) == nil {
			t.Errorf("输入 %q 归一化为 %q 后仍应被拒绝", in, e)
		}
	}
}

// 归一化只应去掉首尾普通空格；含控制字符的输入必须被校验拒绝，
// 而不是被静默"洗"成合法地址。
func TestControlCharsRejectedNotTrimmed(t *testing.T) {
	c := Config{Auth: AuthConfig{DefaultDomain: "test.com"}}
	bad := map[string]string{
		"lisi@test.com\t":   "尾部制表符",
		"\tlisi@test.com":   "前导制表符",
		"lisi@test.com\n":   "尾部换行",
		"lisi@test.com\r\n": "尾部 CRLF",
		"\nlisi@test.com":   "前导换行",
		"lisi\t":            "纯邮箱名带制表符",
		"lisi\n":            "纯邮箱名带换行",
		"lisi@test.com\x00": "尾部空字符",
	}
	for in, why := range bad {
		got := c.NormalizeEmail(in)
		if err := ValidateEmail(got); err == nil {
			t.Errorf("%s：输入 %q 归一化为 %q 后仍被放行", why, in, got)
		}
	}
	// 普通空格属于常见的复制粘贴噪声，去掉后应正常放行
	for _, in := range []string{" lisi@test.com", "lisi@test.com ", "  lisi  "} {
		got := c.NormalizeEmail(in)
		if err := ValidateEmail(got); err != nil {
			t.Errorf("输入 %q 归一化为 %q 后应放行: %v", in, got, err)
		}
	}
}
