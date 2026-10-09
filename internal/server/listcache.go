package server

import (
	"context"
	"sync"
	"time"

	"github.com/xiaoxin2016/goweb/internal/storage"
)

// 目录列表缓存。
//
// 对象存储的 ListObjectsV2 只能按游标顺序翻页，既不能跳到第 N 页，也不返回总数，
// 而“显示总页数”与“按文件名搜索”都需要整个目录的清单——所以只能在服务端完整
// 列举一次（1 万个对象约 10 次请求），再在内存里过滤、分页。为了翻页、切换每页
// 条数、搜索时不必每次都重新全量列举，把列举结果短暂缓存起来：
//
//   - 不带任何查询参数地打开目录（进入目录、刷新第一页）总是重新列举，
//     保证外部（其他工具）写入的文件能被看到
//   - 任何经本服务完成的写操作（上传、删除、新建文件夹、重命名）都会清空缓存
//   - 其余情况最多沿用 listCacheTTL 之前的结果
const (
	listCacheTTL = 30 * time.Second
	// listCacheMaxDirs 最多缓存的目录数，超出时淘汰最早的一项
	listCacheMaxDirs = 32
	// listCacheMaxEntries 单个目录超过该条目数时不缓存，避免占用过多内存
	listCacheMaxEntries = 200_000
)

type listCache struct {
	mu  sync.Mutex
	gen uint64 // 每次失效自增；列举期间发生过写操作的结果不会被写回
	m   map[string]listCacheItem
}

type listCacheItem struct {
	entries []storage.Entry
	at      time.Time
}

// list 返回目录 dir 的完整清单。fresh 为 true 时跳过缓存直接列举。
// key 须区分对象存储配置（配置变更后旧缓存自然失效）。
func (c *listCache) list(ctx context.Context, cli *storage.Client, key, dir string, fresh bool) ([]storage.Entry, error) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]listCacheItem{}
	}
	if it, ok := c.m[key]; ok && !fresh && time.Since(it.at) < listCacheTTL {
		c.mu.Unlock()
		return it.entries, nil
	}
	gen := c.gen
	c.mu.Unlock()

	entries, err := cli.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	if len(entries) > listCacheMaxEntries {
		return entries, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return entries, nil // 列举期间有写操作，结果可能已过时，不写回缓存
	}
	if _, ok := c.m[key]; !ok && len(c.m) >= listCacheMaxDirs {
		c.evictLocked()
	}
	c.m[key] = listCacheItem{entries: entries, at: time.Now()}
	return entries, nil
}

// evictLocked 先清掉过期项；仍然满员时淘汰最早写入的一项。
func (c *listCache) evictLocked() {
	var oldestKey string
	var oldest time.Time
	for k, it := range c.m {
		if time.Since(it.at) >= listCacheTTL {
			delete(c.m, k)
			continue
		}
		if oldestKey == "" || it.at.Before(oldest) {
			oldestKey, oldest = k, it.at
		}
	}
	if len(c.m) >= listCacheMaxDirs && oldestKey != "" {
		delete(c.m, oldestKey)
	}
}

// invalidate 清空全部缓存。写操作很少，整体清空比精确计算受影响的目录更不易出错
// （例如上传文件夹会在当前目录下凭空多出子目录）。
func (c *listCache) invalidate() {
	c.mu.Lock()
	c.gen++
	c.m = nil
	c.mu.Unlock()
}
