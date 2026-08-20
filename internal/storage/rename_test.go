package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/xiaoxin2016/goweb/internal/config"
)

func renameClient(t *testing.T) *Client {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("test-bucket"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	cli, err := New(config.S3Config{
		Endpoint:  srv.URL,
		AccessKey: "ak", SecretKey: "sk",
		Bucket: "test-bucket", RootPrefix: "share", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cli
}

func mustRead(t *testing.T, c *Client, rel string) string {
	t.Helper()
	obj, err := c.Download(context.Background(), rel)
	if err != nil {
		t.Fatalf("下载 %s: %v", rel, err)
	}
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRenameFile(t *testing.T) {
	c := renameClient(t)
	ctx := context.Background()
	if err := c.Upload(ctx, "docs/旧文件 名.txt", strings.NewReader("内容")); err != nil {
		t.Fatal(err)
	}
	// 含中文与空格的名称也要能改名（copy-source 需要转义）
	if err := c.Rename(ctx, "docs/旧文件 名.txt", "docs/新文件.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := mustRead(t, c, "docs/新文件.txt"); got != "内容" {
		t.Errorf("改名后内容 = %q", got)
	}
	if ok, _ := c.Exists(ctx, "docs/旧文件 名.txt"); ok {
		t.Error("源文件应已删除")
	}
}

func TestRenameDirRecursive(t *testing.T) {
	c := renameClient(t)
	ctx := context.Background()
	files := map[string]string{
		"old/a.txt":       "A",
		"old/sub/b.txt":   "B",
		"old/sub/深/c.txt": "C",
	}
	for k, v := range files {
		if err := c.Upload(ctx, k, strings.NewReader(v)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Rename(ctx, "old/", "新目录/"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	for k, v := range files {
		nk := "新目录/" + strings.TrimPrefix(k, "old/")
		if got := mustRead(t, c, nk); got != v {
			t.Errorf("%s = %q, 期望 %q", nk, got, v)
		}
	}
	if ok, _ := c.Exists(ctx, "old/"); ok {
		t.Error("源目录应已清空")
	}
	// 列表中应只剩新目录
	entries, err := c.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "old" {
			t.Error("根目录下仍能看到 old")
		}
	}
}

func TestRenameGuards(t *testing.T) {
	c := renameClient(t)
	ctx := context.Background()
	if err := c.Upload(ctx, "d/a.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	// 文件与目录类型不一致
	if err := c.Rename(ctx, "d/a.txt", "d/b/"); err == nil {
		t.Error("类型不一致应报错")
	}
	// 移动到自身子目录
	if err := c.Rename(ctx, "d/", "d/sub/"); err == nil {
		t.Error("移动到自身子目录应报错")
	}
	// 源目录不存在
	if err := c.Rename(ctx, "nope/", "other/"); err == nil {
		t.Error("源目录不存在应报错")
	}
	// 同名改名是空操作
	if err := c.Rename(ctx, "d/a.txt", "d/a.txt"); err != nil {
		t.Errorf("同名改名不应报错: %v", err)
	}
}

// gofakes3 未实现 UploadPartCopy（带 partNumber 的 PUT 一律按有 body 的分片上传处理），
// 因此用一个仅实现所需三个接口的 mock 验证分片复制的分片划分与请求序列。
func TestCopyObjectMultipartRanges(t *testing.T) {
	const size = int64(12 << 20) // 12MB

	var (
		mu         sync.Mutex
		ranges     []string
		partNums   []string
		completed  bool
		aborted    bool
		copySrcHdr string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && q.Has("uploads"):
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket>`+
				`<Key>k</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && q.Has("partNumber"):
			copySrcHdr = r.Header.Get("x-amz-copy-source")
			ranges = append(ranges, r.Header.Get("x-amz-copy-source-range"))
			partNums = append(partNums, q.Get("partNumber"))
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<CopyPartResult><ETag>&quot;etag-`+q.Get("partNumber")+`&quot;</ETag>`+
				`<LastModified>2026-01-01T00:00:00.000Z</LastModified></CopyPartResult>`)
		case r.Method == http.MethodPost && q.Has("uploadId"):
			completed = true
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<CompleteMultipartUploadResult><Bucket>test-bucket</Bucket>`+
				`<Key>k</Key><ETag>&quot;final&quot;</ETag></CompleteMultipartUploadResult>`)
		case r.Method == http.MethodDelete && q.Has("uploadId"):
			aborted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	cli, err := New(config.S3Config{
		Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk",
		Bucket: "test-bucket", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	oldMax, oldPart := copyMaxSingle, copyPartSize
	copyMaxSingle, copyPartSize = 1<<20, 5<<20 // 阈值 1MB、分片 5MB → 12MB 分成 5+5+2
	t.Cleanup(func() { copyMaxSingle, copyPartSize = oldMax, oldPart })

	if err := cli.copyObject(context.Background(), "src/big.bin", "dst/big.bin"); err != nil {
		t.Fatalf("copyObject: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantRanges := []string{"bytes=0-5242879", "bytes=5242880-10485759", "bytes=10485760-12582911"}
	if !reflect.DeepEqual(ranges, wantRanges) {
		t.Errorf("分片区间 = %v\n期望 = %v", ranges, wantRanges)
	}
	if !reflect.DeepEqual(partNums, []string{"1", "2", "3"}) {
		t.Errorf("分片编号 = %v", partNums)
	}
	if !completed {
		t.Error("未调用 CompleteMultipartUpload")
	}
	if aborted {
		t.Error("成功路径不应调用 AbortMultipartUpload")
	}
	if want := "test-bucket/src/big.bin"; copySrcHdr != want {
		t.Errorf("copy-source = %q, 期望 %q", copySrcHdr, want)
	}
}

// 分片失败时必须中止分片上传，避免残留碎片占用存储
func TestCopyObjectMultipartAbortsOnError(t *testing.T) {
	var aborted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", strconv.FormatInt(8<<20, 10))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && q.Has("uploads"):
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && q.Has("partNumber"):
			w.WriteHeader(http.StatusInternalServerError) // 分片复制失败
		case r.Method == http.MethodDelete && q.Has("uploadId"):
			aborted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	cli, err := New(config.S3Config{
		Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk",
		Bucket: "test-bucket", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldMax, oldPart := copyMaxSingle, copyPartSize
	copyMaxSingle, copyPartSize = 1<<20, 5<<20
	t.Cleanup(func() { copyMaxSingle, copyPartSize = oldMax, oldPart })

	if err := cli.copyObject(context.Background(), "src/big.bin", "dst/big.bin"); err == nil {
		t.Fatal("分片失败时应返回错误")
	}
	if !aborted {
		t.Error("失败后应调用 AbortMultipartUpload 清理")
	}
}

func TestCopySourceEscaping(t *testing.T) {
	got := copySource("bkt", "share/中 文/a+b.txt")
	want := "bkt/share/%E4%B8%AD%20%E6%96%87/a+b.txt"
	if got != want {
		t.Errorf("copySource = %q, 期望 %q", got, want)
	}
}
