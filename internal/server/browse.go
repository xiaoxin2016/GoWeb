package server

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xiaoxin2016/goweb/internal/storage"
)

// Crumb 面包屑导航项。
type Crumb struct {
	Name string
	URL  string
}

type browseData struct {
	Title    string
	Dir      string // 当前目录相对路径（"" 表示根）
	Crumbs   []Crumb
	Entries  []storage.Entry
	User     string
	IsAdmin  bool
	LoadErr  string // 列目录失败时的提示（如 S3 未配置）
	RootName string
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
		Title:    cfg.Title,
		Dir:      dir,
		Crumbs:   buildCrumbs(dir),
		User:     email,
		IsAdmin:  cfg.IsAdmin(email),
		RootName: cfg.Title,
	}

	cli, err := s.s3Client()
	if err == nil {
		ctx, cancel := opCtx(r)
		defer cancel()
		data.Entries, err = cli.List(ctx, dir)
	}
	if err != nil {
		data.LoadErr = err.Error()
	}
	s.render(w, "browse.html", data)
}

func buildCrumbs(dir string) []Crumb {
	crumbs := []Crumb{{Name: "根目录", URL: "/files/"}}
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

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}
