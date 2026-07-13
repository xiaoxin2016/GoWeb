package server

import "testing"

func TestCleanDir(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"", "", false},
		{"/", "", false},
		{"a", "a/", false},
		{"a/b/", "a/b/", false},
		{"/a/b", "a/b/", false},
		{"..", "", true},
		{"a/../b", "", true},
		{"a//b", "", true},
		{`a\b`, "", true},
	}
	for _, c := range cases {
		got, err := cleanDir(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("cleanDir(%q) = %q, err=%v; 期望 %q, wantErr=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestCleanFile(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"a.txt", "a.txt", false},
		{"/a/b.txt", "a/b.txt", false},
		{"", "", true},
		{"a/", "", true},
		{"../a.txt", "", true},
		{"a/../../b", "", true},
	}
	for _, c := range cases {
		got, err := cleanFile(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("cleanFile(%q) = %q, err=%v; 期望 %q, wantErr=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:          "0 B",
		512:        "512 B",
		1024:       "1.0 KB",
		1536:       "1.5 KB",
		1048576:    "1.0 MB",
		5368709120: "5.0 GB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, 期望 %q", in, got, want)
		}
	}
}
