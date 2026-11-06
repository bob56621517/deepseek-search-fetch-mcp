package openapi

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestCache(t *testing.T, ttl time.Duration, maxBytes int64) *Cache {
	t.Helper()
	cache, err := OpenCache(CacheOptions{
		Path:     filepath.Join(t.TempDir(), "cache.db"),
		TTL:      ttl,
		MaxBytes: maxBytes,
	})
	if err != nil {
		t.Fatalf("打开缓存失败: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func TestCacheSearchRoundTrip(t *testing.T) {
	cache := newTestCache(t, 30*time.Minute, 1<<20)
	ctx := context.Background()

	err := cache.PutSearch(ctx, "q1", []CachedPage{
		{URL: "https://example.com/a#section", Title: "A", Content: "hello world", Summary: "short summary"},
		{URL: "https://example.com/b", Title: "B", Summary: "only summary"},
	})
	if err != nil {
		t.Fatalf("写入缓存失败: %v", err)
	}

	summary, ok, err := cache.GetSummary(ctx, "https://example.com/a")
	if err != nil || !ok {
		t.Fatalf("摘要应命中: ok=%v err=%v", ok, err)
	}
	if summary.Summary != "short summary" || summary.SummaryQuery != "q1" || summary.ContentLength != 11 {
		t.Fatalf("摘要内容不符: %#v", summary)
	}

	content, ok, err := cache.GetContent(ctx, "https://EXAMPLE.com/a#other")
	if err != nil || !ok {
		t.Fatalf("正文应命中且忽略 fragment/host 大小写: ok=%v err=%v", ok, err)
	}
	if content.Content != "hello world" || !content.HasSummary {
		t.Fatalf("正文内容不符: %#v", content)
	}

	if _, ok, _ := cache.GetContent(ctx, "https://example.com/b"); ok {
		t.Fatal("空正文不应视为缓存命中")
	}
	if _, ok, _ := cache.GetSummary(ctx, "https://example.com/missing"); ok {
		t.Fatal("不存在的 URL 不应命中")
	}
}

func TestCacheFetchPreferredOverSearch(t *testing.T) {
	cache := newTestCache(t, 30*time.Minute, 1<<20)
	ctx := context.Background()
	const url = "https://example.com/article"

	if err := cache.PutSearch(ctx, "q1", []CachedPage{{
		URL: url, Title: "T1", Content: "search body", Summary: "summary1",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := cache.PutFetch(ctx, CachedPage{URL: url, Title: "T2", Content: "fetch body longer"}, false); err != nil {
		t.Fatal(err)
	}

	page, ok, err := cache.GetContent(ctx, url)
	if err != nil || !ok {
		t.Fatalf("正文应命中: ok=%v err=%v", ok, err)
	}
	if page.Content != "fetch body longer" || page.Title != "T2" || !page.HasSummary || page.Summary != "summary1" {
		t.Fatalf("fetch 结果应保留并携带旧摘要: %#v", page)
	}

	// 后续搜索不应把较短的 search 正文覆盖到 fetch 正文上，但摘要应更新。
	if err := cache.PutSearch(ctx, "q2", []CachedPage{{
		URL: url, Title: "T3", Content: "search body 2", Summary: "summary2",
	}}); err != nil {
		t.Fatal(err)
	}
	page, _, _ = cache.GetContent(ctx, url)
	if page.Content != "fetch body longer" {
		t.Fatalf("search 不应覆盖 fetch 正文: %#v", page)
	}
	summary, ok, _ := cache.GetSummary(ctx, url)
	if !ok || summary.Summary != "summary2" || summary.SummaryQuery != "q2" {
		t.Fatalf("摘要应更新为最新搜索: %#v", summary)
	}
}

func TestCacheTTL(t *testing.T) {
	cache := newTestCache(t, 30*time.Minute, 1<<20)
	clock := &fakeClock{now: time.Now()}
	cache.clock = clock.Now
	ctx := context.Background()

	if err := cache.PutSearch(ctx, "q", []CachedPage{{
		URL: "https://example.com/a", Title: "A", Content: "body", Summary: "summary",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := cache.GetSummary(ctx, "https://example.com/a"); !ok {
		t.Fatal("未过期时应命中")
	}

	clock.Advance(31 * time.Minute)
	if _, ok, _ := cache.GetSummary(ctx, "https://example.com/a"); ok {
		t.Fatal("过期摘要不应命中")
	}
	if _, ok, _ := cache.GetContent(ctx, "https://example.com/a"); ok {
		t.Fatal("过期正文不应命中")
	}
}

func TestCacheIndependentTimestamps(t *testing.T) {
	cache := newTestCache(t, 30*time.Minute, 1<<20)
	clock := &fakeClock{now: time.Now()}
	cache.clock = clock.Now
	ctx := context.Background()
	const url = "https://example.com/a"

	if err := cache.PutSearch(ctx, "q", []CachedPage{{
		URL: url, Title: "A", Content: "old body", Summary: "summary",
	}}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Minute)
	if err := cache.PutFetch(ctx, CachedPage{URL: url, Title: "A", Content: "new body"}, false); err != nil {
		t.Fatal(err)
	}

	// T0+31m：摘要已过期，正文在 T0+20m 刷新，仍然有效。
	clock.Advance(11 * time.Minute)
	if _, ok, _ := cache.GetSummary(ctx, url); ok {
		t.Fatal("摘要应按自己的时间戳过期")
	}
	page, ok, _ := cache.GetContent(ctx, url)
	if !ok || page.Content != "new body" || page.HasSummary {
		t.Fatalf("正文仍应有效且摘要已过期: %#v ok=%v", page, ok)
	}
}

func TestCacheClearSummaryOnRefresh(t *testing.T) {
	cache := newTestCache(t, 30*time.Minute, 1<<20)
	ctx := context.Background()
	const url = "https://example.com/a"

	if err := cache.PutSearch(ctx, "q", []CachedPage{{
		URL: url, Title: "A", Content: "body", Summary: "summary",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := cache.PutFetch(ctx, CachedPage{URL: url, Title: "A", Content: "fresh body"}, true); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := cache.GetSummary(ctx, url); ok {
		t.Fatal("强制刷新后摘要应被清除")
	}
	if page, ok, _ := cache.GetContent(ctx, url); !ok || page.Content != "fresh body" {
		t.Fatalf("强制刷新后正文应更新: %#v ok=%v", page, ok)
	}
}

func TestCacheFlush(t *testing.T) {
	cache := newTestCache(t, 30*time.Minute, 1<<20)
	ctx := context.Background()

	if err := cache.PutSearch(ctx, "q", []CachedPage{{
		URL: "https://example.com/a", Title: "A", Content: "body", Summary: "summary",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Flush(ctx); err != nil {
		t.Fatalf("清空缓存失败: %v", err)
	}
	if _, ok, _ := cache.GetSummary(ctx, "https://example.com/a"); ok {
		t.Fatal("清空后不应命中摘要")
	}
	if _, ok, _ := cache.GetContent(ctx, "https://example.com/a"); ok {
		t.Fatal("清空后不应命中正文")
	}
}

func TestCacheEviction(t *testing.T) {
	// 页级容量以空库基线（约 24 KiB）为起点，用小阈值分别验证：
	// 写入足够大的正文后触发淘汰，且最久未使用的条目先被删除。
	const maxBytes = 384 << 10
	cache := newTestCache(t, time.Hour, maxBytes)
	clock := &fakeClock{now: time.Now()}
	cache.clock = clock.Now
	ctx := context.Background()

	const count = 32
	body := strings.Repeat("x", 16<<10)
	urls := make([]string, 0, count)
	for i := 0; i < count; i++ {
		urls = append(urls, fmt.Sprintf("https://example.com/%d", i))
	}
	for _, url := range urls {
		clock.Advance(time.Second)
		if err := cache.PutSearch(ctx, "q", []CachedPage{{
			URL: url, Title: "t", Content: body,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cache.Prune(ctx); err != nil {
		t.Fatalf("清理失败: %v", err)
	}

	live, err := cache.liveSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live > maxBytes {
		t.Fatalf("清理后仍超出容量: live=%d max=%d", live, maxBytes)
	}
	if _, ok, _ := cache.GetContent(ctx, urls[0]); ok {
		t.Fatal("最久未使用的条目应被淘汰")
	}
	if _, ok, _ := cache.GetContent(ctx, urls[count-1]); !ok {
		t.Fatal("最新写入的条目不应被淘汰")
	}
}

func TestPutSearchSkipsEmptyEntry(t *testing.T) {
	cache := newTestCache(t, time.Minute, 1<<20)
	ctx := context.Background()

	if err := cache.PutSearch(ctx, "q", []CachedPage{{
		URL: "https://example.com/empty", Title: "只有标题",
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	var count int
	if err := cache.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM web_cache WHERE url = ?`, "https://example.com/empty").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("空正文且空摘要不应写入缓存，实际 %d 行", count)
	}
}

func TestClearSummaryKeepsContent(t *testing.T) {
	cache := newTestCache(t, time.Minute, 1<<20)
	ctx := context.Background()
	const url = "https://example.com/a"

	if err := cache.PutSearch(ctx, "q", []CachedPage{{
		URL: url, Title: "标题", Content: "正文内容", Summary: "摘要",
	}}); err != nil {
		t.Fatalf("预热缓存失败: %v", err)
	}
	if err := cache.ClearSummary(ctx, url); err != nil {
		t.Fatalf("清除摘要失败: %v", err)
	}
	if _, ok, err := cache.GetSummary(ctx, url); err != nil || ok {
		t.Fatalf("摘要应被清除: ok=%v err=%v", ok, err)
	}
	if page, ok, err := cache.GetContent(ctx, url); err != nil || !ok || page.Content != "正文内容" {
		t.Fatalf("正文应保留: ok=%v err=%v %#v", ok, err, page)
	}

	var size int64
	if err := cache.db.QueryRowContext(ctx,
		`SELECT size_bytes FROM web_cache WHERE url = ?`, url).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if want := int64(len("标题") + len("正文内容")); size != want {
		t.Fatalf("清除摘要后 size_bytes 应为 %d，实际 %d", want, size)
	}
}

func TestPruneDeletesManyVictims(t *testing.T) {
	// 大缓存需要多轮淘汰：起始批量会逐轮翻倍，直到降至目标水位。
	const maxBytes = 1 << 20
	cache := newTestCache(t, time.Hour, maxBytes)
	ctx := context.Background()

	const count = 200
	body := strings.Repeat("x", 8<<10)
	pages := make([]CachedPage, 0, count)
	for i := 0; i < count; i++ {
		pages = append(pages, CachedPage{
			URL:     fmt.Sprintf("https://example.com/%d", i),
			Title:   "t",
			Content: body,
		})
	}
	if err := cache.PutSearch(ctx, "q", pages); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := cache.Prune(ctx); err != nil {
		t.Fatalf("批量清理失败: %v", err)
	}
	live, err := cache.liveSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live > maxBytes {
		t.Fatalf("清理后仍超出容量: live=%d max=%d", live, maxBytes)
	}
	var remaining int
	if err := cache.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_cache`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining == 0 || remaining >= count {
		t.Fatalf("批量淘汰应删除部分条目，剩余 %d/%d", remaining, count)
	}
}

func TestLiveSizeBaseline(t *testing.T) {
	cache := newTestCache(t, time.Minute, 1<<20)
	live, err := cache.liveSize(context.Background())
	if err != nil {
		t.Fatalf("读取活跃大小失败: %v", err)
	}
	if live <= 0 {
		t.Fatalf("空库的活跃大小应大于 0，实际 %d", live)
	}
}

func TestPruneEmptyCacheTerminates(t *testing.T) {
	// maxBytes 小于空库基线时会进入超限分支，但没有可删的行，必须正常退出。
	cache := newTestCache(t, time.Minute, 1)
	ctx := context.Background()
	if err := cache.Prune(ctx); err != nil {
		t.Fatalf("空表清理失败: %v", err)
	}
}

func TestPruneDeletesExpiredEntries(t *testing.T) {
	cache := newTestCache(t, 30*time.Minute, 1<<20)
	clock := &fakeClock{now: time.Now()}
	cache.clock = clock.Now
	ctx := context.Background()

	if err := cache.PutSearch(ctx, "q", []CachedPage{
		{URL: "https://example.com/a", Title: "A", Content: "body", Summary: "summary"},
		{URL: "https://example.com/b", Title: "B", Content: "body", Summary: "summary"},
	}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(31 * time.Minute)
	if err := cache.Prune(ctx); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	var remaining int
	if err := cache.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_cache`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("过期条目应被删除，剩余 %d 行", remaining)
	}
}

func TestNilCache(t *testing.T) {
	var cache *Cache
	ctx := context.Background()
	if _, ok, err := cache.GetSummary(ctx, "https://example.com/a"); err != nil || ok {
		t.Fatalf("nil 缓存应安全返回未命中: ok=%v err=%v", ok, err)
	}
	if _, ok, err := cache.GetContent(ctx, "https://example.com/a"); err != nil || ok {
		t.Fatalf("nil 缓存应安全返回未命中: ok=%v err=%v", ok, err)
	}
	if err := cache.PutSearch(ctx, "q", []CachedPage{{URL: "https://example.com/a"}}); err != nil {
		t.Fatalf("nil 缓存写入应安全: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("nil 缓存关闭应安全: %v", err)
	}
}
