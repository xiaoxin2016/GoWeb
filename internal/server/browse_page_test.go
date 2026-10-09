package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/xiaoxin2016/goweb/internal/config"
	"github.com/xiaoxin2016/goweb/internal/storage"
)

// browseFixture 一个连着内存 S3 的服务实例：根目录下有 2 个目录（其中“规范文档”只读）
// 与 120 个文件 f001.txt…f120.txt，共 122 项。lists 统计 ListObjectsV2 请求次数。
type browseFixture struct {
	srv         *Server
	admin, user *http.Cookie
	lists       *atomic.Int64
}

func newBrowseFixture(t *testing.T) *browseFixture {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	var lists atomic.Int64
	fake := gofakes3.New(backend).Server()
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			lists.Add(1)
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(s3srv.Close)

	store, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) {
		c.S3 = config.S3Config{Endpoint: s3srv.URL, AccessKey: "k", SecretKey: "s", Bucket: "b", RootPrefix: "share", PathStyle: true}
		c.Auth.AdminEmails = []string{"admin@test.com"}
		c.Auth.AllowedEmails = []string{"*@test.com"}
		c.Auth.ReadOnlyDirs = []string{"规范文档"}
	}); err != nil {
		t.Fatal(err)
	}
	srv, err := New(store, Options{IgnoreEmail: true})
	if err != nil {
		t.Fatal(err)
	}

	cli, err := storage.New(store.Get().S3)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, d := range []string{"规范文档/", "设计稿/"} {
		if err := cli.Mkdir(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 120; i++ {
		if err := cli.Upload(ctx, fmt.Sprintf("f%03d.txt", i), strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}

	f := &browseFixture{srv: srv, lists: &lists}
	f.admin = login(t, srv, "admin@test.com")
	f.user = login(t, srv, "user@test.com")
	lists.Store(0)
	return f
}

// get 以给定身份请求页面，可附加额外 Cookie。
func (f *browseFixture) get(t *testing.T, target string, who *http.Cookie, extra ...*http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(who)
	for _, c := range extra {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

var rowPathRe = regexp.MustCompile(`<tr data-path="([^"]*)"`)

func rowPaths(body string) []string {
	var out []string
	for _, m := range rowPathRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestBrowsePagination(t *testing.T) {
	f := newBrowseFixture(t)

	cases := []struct {
		name, target string
		cookie       *http.Cookie
		wantRows     int
		wantFirst    string
		wantPager    string
	}{
		{"默认每页 50 项", "/files/", nil, 50, "规范文档/", `value="1" inputmode="numeric" aria-label="跳转到页码"> / 3 页`},
		{"第 2 页（每页 20）", "/files/?size=20&page=2", nil, 20, "f019.txt", `value="2" inputmode="numeric" aria-label="跳转到页码"> / 7 页`},
		{"末页只有余数", "/files/?size=20&page=7", nil, 2, "f119.txt", `/ 7 页`},
		{"页码越界钳制到末页", "/files/?size=100&page=99", nil, 22, "f099.txt", `value="2" inputmode="numeric"`},
		{"非法页码回到第 1 页", "/files/?size=100&page=-3", nil, 100, "规范文档/", `value="1" inputmode="numeric"`},
		{"非法每页条数回到默认", "/files/?size=7", nil, 50, "规范文档/", `/ 3 页`},
		{"Cookie 记住每页条数（只有 1 页时不显示翻页控件）", "/files/", &http.Cookie{Name: pageSizeCookie, Value: "500"}, 122, "规范文档/", `<option value="500" selected>`},
		{"查询参数优先于 Cookie", "/files/?size=20", &http.Cookie{Name: pageSizeCookie, Value: "500"}, 20, "规范文档/", `/ 7 页`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var extra []*http.Cookie
			if tc.cookie != nil {
				extra = append(extra, tc.cookie)
			}
			body := f.get(t, tc.target, f.user, extra...)
			rows := rowPaths(body)
			if len(rows) != tc.wantRows {
				t.Errorf("应渲染 %d 行，实际 %d", tc.wantRows, len(rows))
			}
			if len(rows) > 0 && rows[0] != tc.wantFirst {
				t.Errorf("首行应为 %q，实际 %q", tc.wantFirst, rows[0])
			}
			if !strings.Contains(body, tc.wantPager) {
				t.Errorf("分页栏应包含 %q", tc.wantPager)
			}
		})
	}
}

func TestBrowseSearch(t *testing.T) {
	f := newBrowseFixture(t)

	// 不区分大小写：只有 f110…f119 的名称包含 “f11”
	body := f.get(t, "/files/?q=F11&size=20", f.user)
	if rows := rowPaths(body); len(rows) != 10 || rows[0] != "f110.txt" || rows[9] != "f119.txt" {
		t.Errorf("搜索 F11 应命中 f110…f119 共 10 项，实际 %v", rows)
	}
	if !strings.Contains(body, "匹配 <b>10</b> / 122 项") || strings.Contains(body, `class="pager-ctrl"`) {
		t.Error("应展示匹配数与总数")
	}
	// 翻页链接需带上搜索词，否则翻页后结果会丢
	body = f.get(t, "/files/?q=f0&size=20", f.user)
	if !strings.Contains(body, `href="/files/?page=2&amp;q=f0&amp;size=20"`) {
		t.Error("下一页链接应保留搜索词与每页条数")
	}
	// 目录同样参与搜索
	if rows := rowPaths(f.get(t, "/files/?q=规范", f.user)); len(rows) != 1 || rows[0] != "规范文档/" {
		t.Errorf("搜索“规范”应命中目录，实际 %v", rows)
	}
	// 无匹配时给出明确提示，而不是“此目录为空”
	body = f.get(t, "/files/?q=nothing", f.user)
	if !strings.Contains(body, "没有名称包含“nothing”的文件或文件夹") || strings.Contains(body, "此目录为空") {
		t.Error("无匹配时应提示未找到")
	}
	// 搜索词会原样回填，且经过转义
	body = f.get(t, `/files/?q=%22%3E%3Cscript%3E`, f.user)
	if strings.Contains(body, `"><script>`) {
		t.Error("搜索词必须转义后再输出")
	}
}

// 翻页、搜索复用缓存；直接打开目录与任何写操作都会重新列举
func TestBrowseListCache(t *testing.T) {
	f := newBrowseFixture(t)
	step := func(name, target string, wantLists int64) {
		t.Helper()
		before := f.lists.Load()
		f.get(t, target, f.user)
		if got := f.lists.Load() - before; got != wantLists {
			t.Errorf("%s：应发起 %d 次列举，实际 %d", name, wantLists, got)
		}
	}
	step("打开目录", "/files/", 1)
	step("翻页", "/files/?page=2", 0)
	step("切换每页条数", "/files/?size=20&page=5", 0)
	step("搜索", "/files/?q=f1", 0)
	step("再次直接打开目录", "/files/", 1)

	if rec := postJSON(t, f.srv, "/api/mkdir", `{"dir":"","name":"新目录"}`, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("mkdir: %d %s", rec.Code, rec.Body)
	}
	before := f.lists.Load()
	body := f.get(t, "/files/?q=新目录", f.user)
	if f.lists.Load()-before != 1 {
		t.Error("写操作后应重新列举")
	}
	if rows := rowPaths(body); len(rows) != 1 || rows[0] != "新目录/" {
		t.Errorf("写操作后应立即看到新目录，实际 %v", rows)
	}
}

// “只读”标签与是否有写权限无关：管理员也要看到哪些目录对普通用户只读
func TestReadOnlyBadgeVisibleToAdmin(t *testing.T) {
	f := newBrowseFixture(t)
	badge := regexp.MustCompile(`<span class="badge-readonly" title="([^"]*)">只读</span>`)

	for _, tc := range []struct {
		who   *http.Cookie
		title string
	}{
		{f.user, "只读目录：仅管理员可上传和删除"},
		{f.admin, "对普通用户只读：普通用户仅可浏览和下载，管理员不受限制"},
	} {
		m := badge.FindAllStringSubmatch(f.get(t, "/files/", tc.who), -1)
		if len(m) != 1 || m[0][1] != tc.title {
			t.Errorf("根目录应恰好有 1 个只读标签，提示为 %q，实际 %v", tc.title, m)
		}
	}

	// 进入只读目录：管理员看到提示（且仍可上传），普通用户看到只读说明
	adminBody := f.get(t, "/files/规范文档/", f.admin)
	if !strings.Contains(adminBody, "此目录对普通用户只读") || !strings.Contains(adminBody, `id="btn-upload"`) {
		t.Error("管理员在只读目录中应看到提示并保留上传按钮")
	}
	userBody := f.get(t, "/files/规范文档/", f.user)
	if strings.Contains(userBody, "此目录对普通用户只读") || strings.Contains(userBody, `id="btn-upload"`) {
		t.Error("普通用户在只读目录中不应看到管理员提示或上传按钮")
	}
}

func TestPaginate(t *testing.T) {
	for _, tc := range []struct{ n, page, size, cur, pages, lo, hi int }{
		{0, 1, 50, 1, 1, 0, 0},
		{10000, 3, 50, 3, 200, 100, 150},
		{10000, 201, 50, 200, 200, 9950, 10000},
		{10000, 0, 500, 1, 20, 0, 500},
		{101, 3, 50, 3, 3, 100, 101},
	} {
		cur, pages, lo, hi := paginate(tc.n, tc.page, tc.size)
		if cur != tc.cur || pages != tc.pages || lo != tc.lo || hi != tc.hi {
			t.Errorf("paginate(%d,%d,%d) = %d,%d,%d,%d，期望 %d,%d,%d,%d",
				tc.n, tc.page, tc.size, cur, pages, lo, hi, tc.cur, tc.pages, tc.lo, tc.hi)
		}
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 10000: "10,000", 1234567: "1,234,567", -1500: "-1,500"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q，期望 %q", n, got, want)
		}
	}
}
