package storage

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/xiaoxin2016/goweb/internal/config"
)

// newTestClient 启动一个内存 S3 服务并返回指向它的客户端。
func newTestClient(t *testing.T, root string) *Client {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("test-bucket"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)

	cli, err := New(config.S3Config{
		Endpoint:   srv.URL,
		AccessKey:  "test",
		SecretKey:  "test",
		Bucket:     "test-bucket",
		RootPrefix: root,
		PathStyle:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cli
}

func TestEndToEnd(t *testing.T) {
	cli := newTestClient(t, "share/files")
	ctx := context.Background()

	if err := cli.Test(ctx); err != nil {
		t.Fatalf("Test(): %v", err)
	}

	// 上传两个文件和一个子目录中的文件
	for path, content := range map[string]string{
		"hello.txt":      "hello world",
		"b.bin":          "binary",
		"docs/notes.md":  "# notes",
		"docs/deep/x.md": "deep",
	} {
		if err := cli.Upload(ctx, path, strings.NewReader(content)); err != nil {
			t.Fatalf("Upload(%s): %v", path, err)
		}
	}
	if err := cli.Mkdir(ctx, "empty-dir/"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	// 根目录应包含 2 个文件夹 + 2 个文件，且文件夹排前面
	entries, err := cli.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Path)
	}
	want := []string{"docs/", "empty-dir/", "b.bin", "hello.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("List 根目录 = %v, 期望 %v", names, want)
	}
	for _, e := range entries {
		if e.Path == "hello.txt" && e.Size != int64(len("hello world")) {
			t.Errorf("hello.txt 大小 = %d", e.Size)
		}
	}

	// 子目录列表
	entries, err = cli.List(ctx, "docs/")
	if err != nil {
		t.Fatalf("List docs/: %v", err)
	}
	if len(entries) != 2 || entries[0].Path != "docs/deep/" || entries[1].Path != "docs/notes.md" {
		t.Fatalf("List docs/ = %+v", entries)
	}

	// 下载
	obj, err := cli.Download(ctx, "hello.txt")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	data, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if !bytes.Equal(data, []byte("hello world")) {
		t.Fatalf("Download 内容 = %q", data)
	}

	// 删除单个文件
	if err := cli.Delete(ctx, []string{"b.bin"}); err != nil {
		t.Fatalf("Delete 文件: %v", err)
	}
	// 递归删除文件夹
	if err := cli.Delete(ctx, []string{"docs/"}); err != nil {
		t.Fatalf("Delete 文件夹: %v", err)
	}

	entries, err = cli.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	names = names[:0]
	for _, e := range entries {
		names = append(names, e.Path)
	}
	want = []string{"empty-dir/", "hello.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("删除后根目录 = %v, 期望 %v", names, want)
	}
}

// TestRootPrefixIsolation 验证所有操作都被限制在根前缀内。
func TestRootPrefixIsolation(t *testing.T) {
	cli := newTestClient(t, "jail")
	ctx := context.Background()

	if err := cli.Upload(ctx, "a.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	// 另一个指向桶根的客户端应能看到对象位于 jail/ 前缀下
	rootCli := &Client{api: cli.api, bucket: cli.bucket, root: ""}
	entries, err := rootCli.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "jail/" || !entries[0].IsDir {
		t.Fatalf("桶根目录 = %+v, 期望只有 jail/ 文件夹", entries)
	}
}
