// Package storage 封装对 S3 兼容对象存储（AWS S3、阿里云 OSS、MinIO 等）的
// 目录式浏览与文件操作。所有路径均相对于配置的根前缀（RootPrefix）。
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/xiaoxin2016/goweb/internal/config"
)

// Entry 目录中的一项（文件或文件夹）。
type Entry struct {
	Name    string // 显示名称
	Path    string // 相对根前缀的路径；文件夹以 "/" 结尾
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// Object 一个可读取的对象（用于下载）。
type Object struct {
	Body        io.ReadCloser
	Size        int64
	ContentType string
}

// Client 绑定到某个桶与根前缀的存储客户端。
type Client struct {
	api    *s3.Client
	bucket string
	root   string // 已规范化的根前缀，"" 或以 "/" 结尾
}

// New 根据配置构建客户端。
func New(cfg config.S3Config) (*Client, error) {
	if !cfg.Ready() {
		return nil, errors.New("对象存储尚未配置，请先在控制台完成 S3/OSS 设置")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle: cfg.PathStyle,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	return &Client{api: s3.New(opts), bucket: cfg.Bucket, root: cfg.NormalizedRoot()}, nil
}

// key 把相对路径转换成完整的对象键。
func (c *Client) key(rel string) string { return c.root + rel }

// Test 验证配置能否访问桶（列出至多一个对象）。
func (c *Client) Test(ctx context.Context) error {
	_, err := c.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(c.bucket),
		Prefix:  aws.String(c.root),
		MaxKeys: aws.Int32(1),
	})
	return err
}

// List 列出某个目录（dir 为 "" 或以 "/" 结尾的相对路径）下的文件夹与文件。
func (c *Client) List(ctx context.Context, dir string) ([]Entry, error) {
	prefix := c.key(dir)
	var entries []Entry
	seenDirs := map[string]bool{} // 目录去重：不同实现可能同时在两处返回占位对象
	addDir := func(fullKey string) {
		rel := strings.TrimPrefix(fullKey, c.root)
		if rel == "" || seenDirs[rel] {
			return
		}
		seenDirs[rel] = true
		entries = append(entries, Entry{
			Name:  path.Base(strings.TrimSuffix(rel, "/")),
			Path:  rel,
			IsDir: true,
		})
	}
	p := s3.NewListObjectsV2Paginator(c.api, &s3.ListObjectsV2Input{
		Bucket:    aws.String(c.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cp := range page.CommonPrefixes {
			addDir(aws.ToString(cp.Prefix))
		}
		for _, obj := range page.Contents {
			full := aws.ToString(obj.Key)
			if full == prefix {
				continue // 当前目录自身的占位对象
			}
			// 以 "/" 结尾的键是目录占位对象；部分实现（如 gofakes3）
			// 会把它们放在 Contents 而非 CommonPrefixes 中返回
			if strings.HasSuffix(full, "/") {
				addDir(full)
				continue
			}
			rel := strings.TrimPrefix(full, c.root)
			e := Entry{
				Name: path.Base(rel),
				Path: rel,
				Size: aws.ToInt64(obj.Size),
			}
			if obj.LastModified != nil {
				e.ModTime = *obj.LastModified
			}
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

// Upload 流式上传一个文件到相对路径 rel（大文件自动走分片上传）。
func (c *Client) Upload(ctx context.Context, rel string, body io.Reader) error {
	contentType := mime.TypeByExtension(strings.ToLower(path.Ext(rel)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	uploader := manager.NewUploader(c.api)
	_, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(c.key(rel)),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	return err
}

// Download 打开一个对象用于读取。
func (c *Client) Download(ctx context.Context, rel string) (*Object, error) {
	out, err := c.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.key(rel)),
	})
	if err != nil {
		return nil, err
	}
	return &Object{
		Body:        out.Body,
		Size:        aws.ToInt64(out.ContentLength),
		ContentType: aws.ToString(out.ContentType),
	}, nil
}

// Mkdir 创建目录占位对象（零字节、键以 "/" 结尾）。
func (c *Client) Mkdir(ctx context.Context, relDir string) error {
	_, err := c.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.key(relDir)),
		Body:   strings.NewReader(""),
	})
	return err
}

// Delete 删除若干条目；以 "/" 结尾的路径按文件夹递归删除。
func (c *Client) Delete(ctx context.Context, rels []string) error {
	var keys []string
	for _, rel := range rels {
		if strings.HasSuffix(rel, "/") {
			sub, err := c.listAllKeys(ctx, c.key(rel))
			if err != nil {
				return err
			}
			keys = append(keys, sub...)
		} else {
			keys = append(keys, c.key(rel))
		}
	}
	return c.deleteKeys(ctx, keys)
}

func (c *Client) listAllKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(c.api, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
	}
	return keys, nil
}

func (c *Client) deleteKeys(ctx context.Context, keys []string) error {
	const batch = 1000 // DeleteObjects 单次上限
	for len(keys) > 0 {
		n := min(batch, len(keys))
		ids := make([]types.ObjectIdentifier, 0, n)
		for _, k := range keys[:n] {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
		}
		out, err := c.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("删除 %s 失败: %s", aws.ToString(e.Key), aws.ToString(e.Message))
		}
		keys = keys[n:]
	}
	return nil
}
