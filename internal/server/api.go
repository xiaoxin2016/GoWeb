package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// opCtx 为一次存储操作创建带超时的上下文（上传/下载大文件给足时间）。
func opCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 30*time.Minute)
}

// uploadErr 上传中途出错时先排空剩余请求体再返回错误。
// 否则连接被立即关闭，浏览器只会看到“网络错误”而非真正的错误信息。
func uploadErr(w http.ResponseWriter, r *http.Request, status int, err error) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 256<<20))
	writeErr(w, status, err)
}

// handleUpload 处理文件上传（multipart 流式转发到 S3，不落盘）。
// 目录通过查询参数 dir 指定；支持一次上传多个文件。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	dir, err := cleanDir(r.URL.Query().Get("dir"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cli, err := s.s3Client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		uploadErr(w, r, http.StatusBadRequest, fmt.Errorf("解析上传内容失败: %w", err))
		return
	}

	ctx, cancel := opCtx(r)
	defer cancel()

	var uploaded []string
	var pendingPath string // 文件前置的 path 字段：含子目录的相对路径（文件夹上传）
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			uploadErr(w, r, http.StatusBadRequest, fmt.Errorf("读取上传内容失败: %w", err))
			return
		}
		if part.FormName() == "path" && part.FileName() == "" {
			raw, _ := io.ReadAll(io.LimitReader(part, 4096))
			part.Close()
			pendingPath = strings.ReplaceAll(strings.TrimSpace(string(raw)), `\`, "/")
			continue
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		name := pendingPath
		pendingPath = ""
		if name == "" {
			name = path.Base(part.FileName())
		}
		rel, err := cleanFile(dir + name)
		if err != nil {
			part.Close()
			uploadErr(w, r, http.StatusBadRequest, fmt.Errorf("文件名 %q 非法", name))
			return
		}
		if !s.canWrite(r, rel) {
			part.Close()
			denied := errReadOnly(rel)
			s.auditLog(r, "upload", "/"+rel, denied)
			uploadErr(w, r, http.StatusForbidden, denied)
			return
		}
		if err := cli.Upload(ctx, rel, part); err != nil {
			part.Close()
			s.auditLog(r, "upload", "/"+rel, err)
			uploadErr(w, r, http.StatusBadGateway, fmt.Errorf("上传 %s 失败: %w", name, err))
			return
		}
		part.Close()
		uploaded = append(uploaded, name)
		s.auditLog(r, "upload", "/"+rel, nil)
		log.Printf("用户 %s 上传 %s", s.currentUser(r), rel)
	}
	if len(uploaded) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("未收到任何文件"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploaded": uploaded})
}

// handleDownload 流式下载对象，路径通过查询参数 path 指定。
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	rel, err := cleanFile(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cli, err := s.s3Client()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := opCtx(r)
	defer cancel()
	obj, err := cli.Download(ctx, rel)
	s.auditLog(r, "download", "/"+rel, err)
	if err != nil {
		http.Error(w, "下载失败: "+err.Error(), http.StatusNotFound)
		return
	}
	defer obj.Body.Close()

	name := path.Base(rel)
	ct := obj.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename*=UTF-8''%s`, url.PathEscape(name)))
	if obj.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}
	if _, err := io.Copy(w, obj.Body); err != nil {
		// 客户端中断下载属正常情况，仅记录日志
		log.Printf("下载 %s 传输中断: %v", rel, err)
	}
}

// handleDelete 删除若干文件或文件夹（文件夹路径以 "/" 结尾，递归删除）。
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("请求参数错误"))
		return
	}
	var rels []string
	for _, p := range req.Paths {
		if strings.HasSuffix(p, "/") {
			d, err := cleanDir(p)
			if err != nil || d == "" {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("非法路径 %q", p))
				return
			}
			rels = append(rels, d)
		} else {
			f, err := cleanFile(p)
			if err != nil {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("非法路径 %q", p))
				return
			}
			rels = append(rels, f)
		}
	}
	for _, rel := range rels {
		if !s.canWrite(r, rel) {
			denied := errReadOnly(rel)
			s.auditLog(r, "delete", "/"+rel, denied)
			writeErr(w, http.StatusForbidden, denied)
			return
		}
	}
	cli, err := s.s3Client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	ctx, cancel := opCtx(r)
	defer cancel()
	err = cli.Delete(ctx, rels)
	for _, rel := range rels {
		s.auditLog(r, "delete", "/"+rel, err)
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("用户 %s 删除 %v", s.currentUser(r), rels)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": len(rels)})
}

// handleMkdir 在指定目录下新建文件夹。
func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir  string `json:"dir"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求参数错误"))
		return
	}
	dir, err := cleanDir(req.Dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		writeErr(w, http.StatusBadRequest, errors.New("文件夹名称非法"))
		return
	}
	if !s.canWrite(r, dir+name+"/") {
		writeErr(w, http.StatusForbidden, errReadOnly(dir+name+"/"))
		return
	}
	cli, err := s.s3Client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	ctx, cancel := opCtx(r)
	defer cancel()
	err = cli.Mkdir(ctx, dir+name+"/")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"created": dir + name + "/"})
}

// handleRename 重命名文件或文件夹（在原目录内改名），仅管理员可用。
// 请求体：{"path": "docs/a.txt", "name": "b.txt"}；文件夹路径以 "/" 结尾。
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	// 权限与可见性由 requireAdminAPI(adminStrict) 统一保证：
	// 非管理员在进入本函数之前就已收到与未知路由一致的 404。
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求参数错误"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		writeErr(w, http.StatusBadRequest, errors.New("新名称非法（不能为空或包含斜杠）"))
		return
	}

	isDir := strings.HasSuffix(req.Path, "/")
	var oldRel string
	var err error
	if isDir {
		oldRel, err = cleanDir(req.Path)
		if err == nil && oldRel == "" {
			err = errors.New("不能重命名根目录")
		}
	} else {
		oldRel, err = cleanFile(req.Path)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// 同目录内改名：替换路径最后一段
	parent := ""
	trimmed := strings.TrimSuffix(oldRel, "/")
	if i := strings.LastIndexByte(trimmed, '/'); i >= 0 {
		parent = trimmed[:i+1]
	}
	newRel := parent + name
	if isDir {
		newRel += "/"
	}
	if newRel == oldRel {
		writeJSON(w, http.StatusOK, map[string]string{"renamed": oldRel, "to": newRel})
		return
	}
	if isDir {
		if _, err := cleanDir(newRel); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	} else if _, err := cleanFile(newRel); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	cli, err := s.s3Client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	ctx, cancel := opCtx(r)
	defer cancel()

	srcExists, err := cli.Exists(ctx, oldRel)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("检查源是否存在失败: %w", err))
		return
	}
	if !srcExists {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%q 不存在", path.Base(strings.TrimSuffix(oldRel, "/"))))
		return
	}

	// 目标已存在时拒绝，避免静默覆盖
	exists, err := cli.Exists(ctx, newRel)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("检查目标是否存在失败: %w", err))
		return
	}
	if exists {
		writeErr(w, http.StatusConflict, fmt.Errorf("%q 已存在", name))
		return
	}

	err = cli.Rename(ctx, oldRel, newRel)
	s.auditLog(r, "rename", "/"+oldRel+" → /"+newRel, err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("用户 %s 重命名 %s → %s", s.currentUser(r), oldRel, newRel)
	writeJSON(w, http.StatusOK, map[string]string{"renamed": oldRel, "to": newRel})
}
