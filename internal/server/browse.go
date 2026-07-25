package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xiaoxin2016/goweb/internal/config"
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
	Notice   config.NoticeConfig
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
