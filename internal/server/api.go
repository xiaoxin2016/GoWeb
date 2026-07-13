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
