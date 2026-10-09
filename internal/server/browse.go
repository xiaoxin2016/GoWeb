package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaoxin2016/goweb/internal/config"
	"github.com/xiaoxin2016/goweb/internal/storage"
)

// Crumb 面包屑导航项。
type Crumb struct {
	Name string
	URL  string
}

// viewEntry 列表项的展示模型：条目本身 + 当前用户对它的写权限。
type viewEntry struct {
	storage.Entry
	ReadOnly bool // 当前用户不可在此条目上执行写操作
	// ReadOnlyDir 该条目是被配置为只读的一级目录。与 ReadOnly 不同，它与当前用户
	// 无关：管理员不受只读限制，但同样需要看到哪些目录对普通用户只读。
	ReadOnlyDir bool
}

// 分页。
var pageSizes = []int{20, 50, 100, 500}

const (
	defaultPageSize = 50
	// pageSizeCookie 记住用户选择的每页条数，进入其他目录时沿用
	pageSizeCookie = "goweb_page_size"
	// maxQueryRunes 搜索关键字长度上限
	maxQueryRunes = 100
)

// pageInfo 分页与搜索的展示模型。
type pageInfo struct {
	Page, Pages, Size int
	Total             int // 目录内条目总数
	Matched           int // 匹配搜索的条目数；未搜索时等于 Total
	From, To          int // 当前页条目的序号范围（从 1 起）；无条目时均为 0
	Query             string
	Sizes             []int
	// 翻页链接；不可用（已在首页/末页）时为空
	FirstURL, PrevURL, NextURL, LastURL string
	ClearURL                            string // 清除搜索、回到第一页
}

// parsePageSize 依次取查询参数 size、Cookie，均无效时用默认值。
func parsePageSize(r *http.Request) int {
	raw := r.URL.Query().Get("size")
	if raw == "" {
		if c, err := r.Cookie(pageSizeCookie); err == nil {
			raw = c.Value
		}
	}
	n, _ := strconv.Atoi(raw)
	if slices.Contains(pageSizes, n) {
		return n
	}
	return defaultPageSize
}

// parseQuery 取搜索关键字：去掉首尾空白并截断到上限。
func parseQuery(r *http.Request) string {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) > maxQueryRunes {
		q = string([]rune(q)[:maxQueryRunes])
	}
	return q
}

// filterEntries 返回名称包含 q 的条目（不区分大小写）；q 为空时原样返回。
func filterEntries(entries []storage.Entry, q string) []storage.Entry {
	if q == "" {
		return entries
	}
	q = strings.ToLower(q)
	var out []storage.Entry
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name), q) {
			out = append(out, e)
		}
	}
	return out
}

// paginate 把页码钳制到 [1, pages]，返回当前页在 n 个条目中的切片区间 [lo, hi)。
// 没有条目时也视为 1 页。
func paginate(n, page, size int) (cur, pages, lo, hi int) {
	pages = max(1, (n+size-1)/size)
	cur = min(max(page, 1), pages)
	lo = min((cur-1)*size, n)
	hi = min(lo+size, n)
	return cur, pages, lo, hi
}

// pageURL 构造目录 dir 的分页链接。
func pageURL(dir, q string, size, page int) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	v.Set("size", strconv.Itoa(size))
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	return "/files/" + escapePath(dir) + "?" + v.Encode()
}

type browseData struct {
	Title   string
	Dir     string // 当前目录相对路径（"" 表示根）
	Crumbs  []Crumb
	Entries []viewEntry
	User    string
	IsAdmin bool
	// CanWrite 当前目录是否允许当前用户写入（上传、新建文件夹）
	CanWrite bool
	LoadErr  string // 列目录失败时的提示（如 S3 未配置）
	// DirReadOnly 当前目录位于被配置为只读的一级目录之下（供管理员提示）
	DirReadOnly bool
	Pager       pageInfo
	RootName    string
	Notice      config.NoticeConfig
	// NoticeKey 公告内容的短哈希，供前端记录“已关闭”状态；
	// 公告内容变更后 key 随之改变，会重新展示。
	NoticeKey string
}

// handleBrowse 渲染目录浏览页面。
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	dir, err := cleanDir(strings.TrimPrefix(r.URL.Path, "/files/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cfg := s.cfg.Get()
	email := s.currentUser(r)
	data := browseData{
		Title:     cfg.Title,
		Dir:       dir,
		Crumbs:    buildCrumbs(cfg.Title, dir),
		User:      email,
		IsAdmin:   cfg.IsAdmin(email),
		RootName:  cfg.Title,
		Notice:    cfg.Notice,
		NoticeKey: noticeKey(cfg.Notice),
	}

	// 目录本身的写权限：根目录始终可写（新建一级目录、上传根级文件），
	// 子目录取决于其所属的一级目录是否只读。
	data.CanWrite = dir == "" || cfg.CanWrite(email, dir)
	data.DirReadOnly = dir != "" && cfg.IsReadOnlyDir(config.TopDir(dir))

	cli, err := s.s3Client()
	if err == nil {
		ctx, cancel := opCtx(r)
		defer cancel()
		// 不带查询参数地打开目录（进入目录、刷新首页）时总是重新列举；
		// 翻页、切换每页条数、搜索时复用短期缓存
		fresh := r.URL.RawQuery == ""
		var all []storage.Entry
		all, err = s.lists.list(ctx, cli, strconv.FormatInt(s.cfg.Rev(), 10)+"\x00"+dir, dir, fresh)
		if err == nil {
			data.Pager, data.Entries = s.pageOf(cfg, email, dir, all, r)
		}
	}
	if err != nil {
		data.LoadErr = err.Error()
	}
	s.render(w, "browse.html", data)
}

// pageOf 对完整清单做搜索过滤与分页，只为当前页的条目计算权限。
func (s *Server) pageOf(cfg config.Config, email, dir string, all []storage.Entry, r *http.Request) (pageInfo, []viewEntry) {
	q := parseQuery(r)
	size := parsePageSize(r)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	matched := filterEntries(all, q)
	cur, pages, lo, hi := paginate(len(matched), page, size)

	p := pageInfo{
		Page: cur, Pages: pages, Size: size,
		Total: len(all), Matched: len(matched),
		Query: q, Sizes: pageSizes,
		ClearURL: pageURL(dir, "", size, 1),
	}
	if hi > lo {
		p.From, p.To = lo+1, hi
	}
	if cur > 1 {
		p.FirstURL, p.PrevURL = pageURL(dir, q, size, 1), pageURL(dir, q, size, cur-1)
	}
	if cur < pages {
		p.NextURL, p.LastURL = pageURL(dir, q, size, cur+1), pageURL(dir, q, size, pages)
	}

	entries := make([]viewEntry, 0, hi-lo)
	for _, e := range matched[lo:hi] {
		entries = append(entries, viewEntry{
			Entry:       e,
			ReadOnly:    !cfg.CanWrite(email, e.Path),
			ReadOnlyDir: e.IsDir && dir == "" && cfg.IsReadOnlyDir(config.TopDir(e.Path)),
		})
	}
	return p, entries
}

// buildCrumbs 构造面包屑：根节点显示站点名称，其后是各级子目录。
func buildCrumbs(rootName, dir string) []Crumb {
	crumbs := []Crumb{{Name: rootName, URL: "/files/"}}
	if dir == "" {
		return crumbs
	}
	segs := strings.Split(strings.TrimSuffix(dir, "/"), "/")
	acc := ""
	for _, seg := range segs {
		acc += seg + "/"
		crumbs = append(crumbs, Crumb{Name: seg, URL: "/files/" + escapePath(acc)})
	}
	return crumbs
}

// escapePath 对路径逐段进行 URL 编码，保留分隔符。
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// noticeKey 返回公告内容的短哈希，用于前端区分不同版本的公告。
func noticeKey(n config.NoticeConfig) string {
	sum := sha256.Sum256([]byte(n.Level + "\x00" + n.Text))
	return hex.EncodeToString(sum[:6])
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// thousands 为整数加千位分隔符，如 10000 → "10,000"。
func thousands(n int) string {
	if n < 0 {
		return "-" + thousands(-n)
	}
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
