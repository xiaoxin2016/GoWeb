package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/xiaoxin2016/goweb/internal/config"
)

// TestUploadNoAwsChunked 回归测试：分片上传不得使用 aws-chunked 尾部校验和编码
// （阿里云 OSS 等第三方实现的 UploadPart 不支持，报
// “aws-chunked encoding is not supported with the specified x-amz-content-sha256 value”）。
// 尾部校验和只在 HTTPS 下启用，因此用 TLS 假 S3 服务并在 HTTP 层断言请求头。
func TestUploadNoAwsChunked(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket("test-bucket"); err != nil {
		t.Fatal(err)
	}
	inner := gofakes3.New(backend).Server()

	var mu sync.Mutex
	var offenders []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") ||
			strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			mu.Lock()
			offenders = append(offenders, r.Method+" "+r.URL.RawQuery)
			mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	cli, err := New(config.S3Config{
		Endpoint:  srv.URL,
		AccessKey: "ak", SecretKey: "sk",
		Bucket: "test-bucket", PathStyle: true,
		InsecureTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 12MB 随机数据，超过默认 5MB 分片大小，必然走多分片 UploadPart
	data := make([]byte, 12<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := cli.Upload(ctx, "big.bin", bytes.NewReader(data)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// 小文件（单请求 PutObject）同样不得使用 aws-chunked
	if err := cli.Upload(ctx, "small.txt", strings.NewReader("hello")); err != nil {
		t.Fatalf("Upload small: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(offenders) > 0 {
		t.Fatalf("以下请求使用了 aws-chunked/尾部校验和编码: %v", offenders)
	}

	// 校验上传内容完整
	obj, err := cli.Download(ctx, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("下载内容与上传不一致: len=%d want %d", len(got), len(data))
	}
}
