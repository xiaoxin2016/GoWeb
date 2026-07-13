package storage

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/xiaoxin2016/goweb/internal/config"
)

// TestInsecureTLS 验证自签名证书：默认拒绝，开启 InsecureTLS 后可用。
func TestInsecureTLS(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket("test-bucket"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(gofakes3.New(backend).Server()) // 自签名证书
	t.Cleanup(srv.Close)

	base := config.S3Config{
		Endpoint:  srv.URL,
		AccessKey: "ak", SecretKey: "sk",
		Bucket: "test-bucket", PathStyle: true,
	}

	cli, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Test(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "certificate") {
		t.Fatalf("默认应因证书校验失败, got %v", err)
	}

	base.InsecureTLS = true
	cli, err = New(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Test(context.Background()); err != nil {
		t.Fatalf("开启 InsecureTLS 后应连接成功: %v", err)
	}
}
