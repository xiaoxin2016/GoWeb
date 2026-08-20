// Package storage 封装对 S3 兼容对象存储（AWS S3、阿里云 OSS、MinIO 等）的
// 目录式浏览与文件操作。所有路径均相对于配置的根前缀（RootPrefix）。
package storage

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
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
		// 仅在必要时计算/校验 AWS 风格的校验和：阿里云 OSS、旧版 MinIO 等
		// 第三方实现不支持 x-amz-checksum-* 系列头
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	if cfg.InsecureTLS {
		// 私有云自签名证书场景：跳过证书校验（仅限可信内网）
		opts.HTTPClient = awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
			if t.TLSClientConfig == nil {
				t.TLSClientConfig = &tls.Config{}
			}
			t.TLSClientConfig.InsecureSkipVerify = true
		})
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
	uploader := manager.NewUploader(c.api, func(u *manager.Uploader) {
		// Uploader 有独立于 S3 客户端的校验和开关，默认 WhenSupported 会给
		// UploadPart 附加 CRC32 尾部校验和（aws-chunked 编码），阿里云 OSS 等
		// 第三方实现不支持，报 InvalidArgument；必须在此单独关闭
		u.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
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

// deleteKeys 并发逐个删除对象。
// 刻意不使用 DeleteObjects 批量接口：该接口在 SDK 中强制携带校验和，
// 新版 SDK 只会发送 CRC32（x-amz-checksum-crc32），而阿里云 OSS 等第三方
// 实现要求 Content-MD5，导致 400 MissingArgument。逐个 DeleteObject
// 没有校验和要求，在所有 S3 兼容服务上行为一致。
func (c *Client) deleteKeys(ctx context.Context, keys []string) error {
	const workers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, workers)
	for _, k := range keys {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(key string) {
			defer wg.Done()
			defer func() { <-sem }()
			_, err := c.api.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(c.bucket),
				Key:    aws.String(key),
			})
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("删除 %s 失败: %w", key, err)
				}
				mu.Unlock()
			}
		}(k)
	}
	wg.Wait()
	return firstErr
}

// copyPartSize 大对象分片复制的分片大小。
// copyMaxSingle 是单次 CopyObject 支持的对象大小上限（S3 规定 5GB），
// 超过则改用分片复制（UploadPartCopy）。两者均为变量以便测试调整。
var (
	copyMaxSingle int64 = 5 << 30   // 5GB
	copyPartSize  int64 = 512 << 20 // 512MB
)

// Exists 报告对象是否存在（目录传入以 "/" 结尾的路径）。
func (c *Client) Exists(ctx context.Context, rel string) (bool, error) {
	if strings.HasSuffix(rel, "/") {
		// 目录：只要该前缀下存在任意对象即视为存在
		out, err := c.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:  aws.String(c.bucket),
			Prefix:  aws.String(c.key(rel)),
			MaxKeys: aws.Int32(1),
		})
		if err != nil {
			return false, err
		}
		return aws.ToInt32(out.KeyCount) > 0, nil
	}
	_, err := c.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.key(rel)),
	})
	if err != nil {
		var nf *types.NotFound
		var noKey *types.NoSuchKey
		if errors.As(err, &nf) || errors.As(err, &noKey) {
			return false, nil
		}
		// 部分实现对不存在的对象返回 404 而非具体错误类型
		var respErr *awshttp.ResponseError
		if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Rename 重命名（移动）文件或文件夹。
// oldRel/newRel 同为文件路径，或同为以 "/" 结尾的文件夹路径；
// 文件夹会递归搬迁其下全部对象。对象存储没有原生重命名，实现为复制后删除源。
func (c *Client) Rename(ctx context.Context, oldRel, newRel string) error {
	isDir := strings.HasSuffix(oldRel, "/")
	if isDir != strings.HasSuffix(newRel, "/") {
		return errors.New("重命名的源和目标类型不一致")
	}
	if oldRel == newRel {
		return nil
	}
	if isDir && strings.HasPrefix(newRel, oldRel) {
		return errors.New("不能把文件夹移动到它自己的子目录下")
	}

	if !isDir {
		if err := c.copyObject(ctx, c.key(oldRel), c.key(newRel)); err != nil {
			return err
		}
		return c.deleteKeys(ctx, []string{c.key(oldRel)})
	}

	oldPrefix, newPrefix := c.key(oldRel), c.key(newRel)
	keys, err := c.listAllKeys(ctx, oldPrefix)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("文件夹不存在或为空")
	}
	// 先整体复制，全部成功后再删除源，避免中途失败造成数据丢失
	for _, k := range keys {
		if err := c.copyObject(ctx, k, newPrefix+strings.TrimPrefix(k, oldPrefix)); err != nil {
			return err
		}
	}
	return c.deleteKeys(ctx, keys)
}

// copyObject 在同一桶内复制对象，超过单次复制上限时走分片复制。
func (c *Client) copyObject(ctx context.Context, srcKey, dstKey string) error {
	head, err := c.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(srcKey),
	})
	if err != nil {
		return fmt.Errorf("读取 %s 信息失败: %w", path.Base(srcKey), err)
	}
	size := aws.ToInt64(head.ContentLength)
	if size > copyMaxSingle {
		return c.copyObjectMultipart(ctx, srcKey, dstKey, size, head.ContentType)
	}
	_, err = c.api.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(c.bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(copySource(c.bucket, srcKey)),
	})
	if err != nil {
		return fmt.Errorf("复制 %s 失败: %w", path.Base(srcKey), err)
	}
	return nil
}

// copyObjectMultipart 用 UploadPartCopy 搬迁超过 5GB 的大对象。
func (c *Client) copyObjectMultipart(ctx context.Context, srcKey, dstKey string, size int64, contentType *string) error {
	create, err := c.api.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(dstKey),
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("发起分片复制失败: %w", err)
	}
	uploadID := create.UploadId

	abort := func() {
		_, _ = c.api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(c.bucket), Key: aws.String(dstKey), UploadId: uploadID,
		})
	}

	var parts []types.CompletedPart
	src := copySource(c.bucket, srcKey)
	for start, num := int64(0), int32(1); start < size; num++ {
		end := start + copyPartSize - 1
		if end > size-1 {
			end = size - 1
		}
		out, err := c.api.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
			Bucket:          aws.String(c.bucket),
			Key:             aws.String(dstKey),
			UploadId:        uploadID,
			PartNumber:      aws.Int32(num),
			CopySource:      aws.String(src),
			CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
		})
		if err != nil {
			abort()
			return fmt.Errorf("复制 %s 的第 %d 个分片失败: %w", path.Base(srcKey), num, err)
		}
		parts = append(parts, types.CompletedPart{
			ETag:       out.CopyPartResult.ETag,
			PartNumber: aws.Int32(num),
		})
		start = end + 1
	}

	_, err = c.api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(c.bucket),
		Key:             aws.String(dstKey),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		abort()
		return fmt.Errorf("完成分片复制失败: %w", err)
	}
	return nil
}

// copySource 构造 x-amz-copy-source 的值：逐段转义，保留 "/" 分隔符，
// 使中文与空格等字符能安全放进 HTTP 头。
func copySource(bucket, key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return bucket + "/" + strings.Join(segs, "/")
}
