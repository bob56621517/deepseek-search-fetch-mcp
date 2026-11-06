package openapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// DefaultCacheTTL 是缓存条目的默认有效期。
	DefaultCacheTTL = 30 * time.Minute
	// DefaultCacheMaxBytes 是缓存活跃页大小的默认上限，不代表磁盘占用。
	DefaultCacheMaxBytes int64 = 1 << 30
	// cachePruneInterval 限制自动清理的最小间隔。
	cachePruneInterval = time.Minute
	// pruneBatchMax 限制单轮容量淘汰删除的行数，避免一次删掉过多条目。
	// 实际批量按“需要释放的比例”估算，通常远小于该上限。
	pruneBatchMax = 4096
	// pruneMaxRounds 限制单次 Prune 的淘汰轮数，避免长期占用连接。
	pruneMaxRounds = 32
	// pruneTargetPercent 是容量淘汰的目标水位（maxBytes 的百分比）。
	pruneTargetPercent = 95
)

// CacheOptions 是 SQLite 缓存的打开参数。
type CacheOptions struct {
	Path     string
	TTL      time.Duration
	MaxBytes int64
	// Logger 用于记录清理失败等非致命错误；为 nil 时使用默认 logger。
	Logger *slog.Logger
}

// CachedPage 是缓存中的一个网页条目。
type CachedPage struct {
	URL           string
	Title         string
	Content       string
	ContentLength int
	Summary       string
	SummaryQuery  string
	ContentSource string
	ContentAt     time.Time
	SummaryAt     time.Time
	HasSummary    bool
}

// Cache 是进程内共享、落盘到 SQLite 的网页缓存。
// 方法在缓存为 nil 时也安全，便于在缓存不可用时降级运行。
type Cache struct {
	db       *sql.DB
	ttl      time.Duration
	maxBytes int64
	clock    func() time.Time
	logger   *slog.Logger

	pruneMu   sync.Mutex
	lastPrune time.Time
	// pruning 防止单次 Prune 执行时间超过 cachePruneInterval 时被再次重入。
	pruning atomic.Bool
}

// OpenCache 打开（必要时创建）SQLite 缓存库。
func OpenCache(opts CacheOptions) (*Cache, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("缓存路径不能为空")
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultCacheTTL
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultCacheMaxBytes
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败: %w", err)
	}

	dsn := filepath.ToSlash(opts.Path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开缓存库失败: %w", err)
	}
	// 单连接足以支撑本地缓存，同时避免多连接之间的锁竞争。
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	c := &Cache{db: db, ttl: opts.TTL, maxBytes: opts.MaxBytes, clock: time.Now, logger: logger}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接缓存库失败: %w", err)
	}
	if err := c.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(opts.Path, 0o600)

	// 启动时清理一次过期条目；失败不影响主功能。
	if err := c.Prune(context.Background()); err != nil {
		logger.Warn("缓存清理失败", "err", err)
	}
	c.lastPrune = c.now()
	return c, nil
}

func (c *Cache) initSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS web_cache (
			url            TEXT PRIMARY KEY,
			title          TEXT NOT NULL DEFAULT '',
			content        TEXT NOT NULL DEFAULT '',
			summary        TEXT NOT NULL DEFAULT '',
			summary_query  TEXT NOT NULL DEFAULT '',
			content_source TEXT NOT NULL DEFAULT '',
			content_at     INTEGER NOT NULL DEFAULT 0,
			summary_at     INTEGER NOT NULL DEFAULT 0,
			last_used      INTEGER NOT NULL DEFAULT 0,
			size_bytes     INTEGER NOT NULL DEFAULT 0
		)`,
		// last_used 复合索引覆盖容量淘汰的 ORDER BY last_used, url。
		// 使用新索引名而不是原地修改：CREATE INDEX IF NOT EXISTS 遇到同名旧索引会静默保留旧定义。
		`CREATE INDEX IF NOT EXISTS idx_web_cache_last_used_url ON web_cache(last_used, url)`,
		`DROP INDEX IF EXISTS idx_web_cache_last_used`,
		// TTL 删除按 content_at 做范围扫描，保留该索引。
		`CREATE INDEX IF NOT EXISTS idx_web_cache_content_at ON web_cache(content_at)`,
		// summary_at 的独立索引没有任何查询单独使用，属于冗余。
		`DROP INDEX IF EXISTS idx_web_cache_summary_at`,
	}
	for _, stmt := range statements {
		if _, err := c.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("初始化缓存表失败: %w", err)
		}
	}
	return nil
}

// Close 关闭缓存库。
func (c *Cache) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// Flush 清空所有缓存，并回收 SQLite 文件空间。
func (c *Cache) Flush(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM web_cache`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	return nil
}

// GetSummary 读取未过期的摘要。调用方不需要再判断 TTL。
func (c *Cache) GetSummary(ctx context.Context, rawURL string) (CachedPage, bool, error) {
	if c == nil {
		return CachedPage{}, false, nil
	}
	key := cacheKey(rawURL)
	cutoff := c.cutoff()
	var (
		page      CachedPage
		contentAt int64
		summaryAt int64
	)
	err := c.db.QueryRowContext(ctx, `
		SELECT title, summary, summary_query, length(content), content_at, summary_at
		FROM web_cache
		WHERE url = ? AND summary_at >= ?`, key, cutoff).Scan(
		&page.Title, &page.Summary, &page.SummaryQuery, &page.ContentLength, &contentAt, &summaryAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return CachedPage{}, false, nil
	}
	if err != nil {
		return CachedPage{}, false, err
	}
	if page.Summary == "" {
		return CachedPage{}, false, nil
	}
	page.URL = key
	page.ContentAt = time.Unix(contentAt, 0)
	page.SummaryAt = time.Unix(summaryAt, 0)
	page.HasSummary = true
	c.touch(ctx, key)
	return page, true, nil
}

// GetContent 读取未过期的正文，并顺带返回是否有可用的摘要。
func (c *Cache) GetContent(ctx context.Context, rawURL string) (CachedPage, bool, error) {
	if c == nil {
		return CachedPage{}, false, nil
	}
	key := cacheKey(rawURL)
	cutoff := c.cutoff()
	var (
		page      CachedPage
		contentAt int64
		summaryAt int64
	)
	err := c.db.QueryRowContext(ctx, `
		SELECT title, content, length(content), content_at, summary, summary_at
		FROM web_cache
		WHERE url = ? AND content_at >= ?`, key, cutoff).Scan(
		&page.Title, &page.Content, &page.ContentLength, &contentAt, &page.Summary, &summaryAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return CachedPage{}, false, nil
	}
	if err != nil {
		return CachedPage{}, false, err
	}
	if page.Content == "" {
		return CachedPage{}, false, nil
	}
	page.URL = key
	page.ContentAt = time.Unix(contentAt, 0)
	page.SummaryAt = time.Unix(summaryAt, 0)
	page.HasSummary = page.Summary != "" && summaryAt >= cutoff
	c.touch(ctx, key)
	return page, true, nil
}

// PutSearch 写入一次搜索返回的条目。写入失败由调用方决定是否忽略。
func (c *Cache) PutSearch(ctx context.Context, query string, pages []CachedPage) error {
	if c == nil || len(pages) == 0 {
		return nil
	}
	now := c.now()
	cutoff := c.cutoff()

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, page := range pages {
		if strings.TrimSpace(page.URL) == "" {
			continue
		}
		key := cacheKey(page.URL)
		final, err := mergeSearchEntry(ctx, tx, key, query, page, now, cutoff)
		if err != nil {
			return err
		}
		// 上游偶尔返回既无正文也无摘要的条目，写入只会留下空行。
		if final.Content == "" && final.Summary == "" {
			continue
		}
		if err := upsertEntry(ctx, tx, final, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.maybePrune(ctx)
	return nil
}

// PutFetch 写入一次抓取结果。fetch 的正文比 search 的更完整，总是覆盖正文；
// clearSummary 为 true 时同时丢弃旧摘要（强制刷新场景）。
func (c *Cache) PutFetch(ctx context.Context, page CachedPage, clearSummary bool) error {
	if c == nil || strings.TrimSpace(page.URL) == "" || page.Content == "" {
		return nil
	}
	now := c.now()
	key := cacheKey(page.URL)

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		existing  CachedPage
		summaryAt int64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT title, summary, summary_query, summary_at
		FROM web_cache WHERE url = ?`, key).Scan(
		&existing.Title, &existing.Summary, &existing.SummaryQuery, &summaryAt,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	final := CachedPage{
		URL:           key,
		Title:         page.Title,
		Content:       page.Content,
		ContentSource: "fetch",
		ContentAt:     now,
		Summary:       existing.Summary,
		SummaryQuery:  existing.SummaryQuery,
		SummaryAt:     time.Unix(summaryAt, 0),
	}
	if final.Title == "" {
		final.Title = existing.Title
	}
	if clearSummary || summaryAt < c.cutoff() {
		final.Summary = ""
		final.SummaryQuery = ""
		final.SummaryAt = time.Time{}
	}
	if err := upsertEntry(ctx, tx, final, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.maybePrune(ctx)
	return nil
}

// ClearSummary 丢弃指定 URL 的摘要，保留正文。
// 用于 refresh 时上游返回空正文、无法写入新条目的场景。
// 有意不更新 last_used：正文并未刷新成功，保留旧的 LRU 位置让条目更早被淘汰。
func (c *Cache) ClearSummary(ctx context.Context, rawURL string) error {
	if c == nil {
		return nil
	}
	key := cacheKey(rawURL)
	_, err := c.db.ExecContext(ctx, `
		UPDATE web_cache
		SET summary = '', summary_query = '', summary_at = 0,
		    size_bytes = length(CAST(title AS BLOB)) + length(CAST(content AS BLOB))
		WHERE url = ?`, key)
	return err
}

func mergeSearchEntry(ctx context.Context, tx *sql.Tx, key, query string, page CachedPage, now time.Time, cutoff int64) (CachedPage, error) {
	var (
		existing    CachedPage
		contentAt   int64
		summaryAt   int64
		contentSrc  string
		existingHas bool
	)
	err := tx.QueryRowContext(ctx, `
		SELECT title, content, summary, summary_query, content_source, content_at, summary_at
		FROM web_cache WHERE url = ?`, key).Scan(
		&existing.Title, &existing.Content, &existing.Summary, &existing.SummaryQuery,
		&contentSrc, &contentAt, &summaryAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		existingHas = false
	} else if err != nil {
		return CachedPage{}, err
	} else {
		existingHas = true
		existing.URL = key
		existing.ContentSource = contentSrc
		existing.ContentAt = time.Unix(contentAt, 0)
		existing.SummaryAt = time.Unix(summaryAt, 0)
	}

	final := CachedPage{URL: key, Title: page.Title}
	if page.Content != "" {
		// fetch 的正文更完整；缓存未过期时不要用 search 的正文把它降级。
		keepFetched := existingHas &&
			existing.ContentSource == "fetch" &&
			existing.Content != "" &&
			contentAt >= cutoff
		if keepFetched {
			final.Content = existing.Content
			final.ContentSource = existing.ContentSource
			final.ContentAt = existing.ContentAt
		} else {
			final.Content = page.Content
			final.ContentSource = "search"
			final.ContentAt = now
		}
	} else if existingHas {
		final.Content = existing.Content
		final.ContentSource = existing.ContentSource
		final.ContentAt = existing.ContentAt
	}
	if final.Title == "" {
		final.Title = existing.Title
	}

	if page.Summary != "" {
		final.Summary = page.Summary
		final.SummaryQuery = query
		final.SummaryAt = now
	} else if existingHas && summaryAt >= cutoff {
		final.Summary = existing.Summary
		final.SummaryQuery = existing.SummaryQuery
		final.SummaryAt = existing.SummaryAt
	}
	return final, nil
}

func upsertEntry(ctx context.Context, tx *sql.Tx, page CachedPage, now time.Time) error {
	contentAt := int64(0)
	if !page.ContentAt.IsZero() {
		contentAt = page.ContentAt.Unix()
	}
	summaryAt := int64(0)
	if !page.SummaryAt.IsZero() {
		summaryAt = page.SummaryAt.Unix()
	}
	size := int64(len(page.Title) + len(page.Content) + len(page.Summary))
	_, err := tx.ExecContext(ctx, `
		INSERT INTO web_cache
			(url, title, content, summary, summary_query, content_source, content_at, summary_at, last_used, size_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			title          = excluded.title,
			content        = excluded.content,
			summary        = excluded.summary,
			summary_query  = excluded.summary_query,
			content_source = excluded.content_source,
			content_at     = excluded.content_at,
			summary_at     = excluded.summary_at,
			last_used      = excluded.last_used,
			size_bytes     = excluded.size_bytes`,
		page.URL, page.Title, page.Content, page.Summary, page.SummaryQuery,
		page.ContentSource, contentAt, summaryAt, now.Unix(), size,
	)
	return err
}

func (c *Cache) touch(ctx context.Context, key string) {
	_, _ = c.db.ExecContext(ctx, `UPDATE web_cache SET last_used = ? WHERE url = ?`, c.now().Unix(), key)
}

func (c *Cache) now() time.Time {
	if c == nil || c.clock == nil {
		return time.Now()
	}
	return c.clock()
}

func (c *Cache) cutoff() int64 {
	return c.now().Add(-c.ttl).Unix()
}

func (c *Cache) maybePrune(ctx context.Context) {
	if c == nil {
		return
	}
	now := c.now()
	c.pruneMu.Lock()
	if now.Sub(c.lastPrune) < cachePruneInterval {
		c.pruneMu.Unlock()
		return
	}
	c.lastPrune = now
	c.pruneMu.Unlock()
	// 单次 Prune 可能超过 cachePruneInterval；用非阻塞标志避免并发重入。
	if !c.pruning.CompareAndSwap(false, true) {
		return
	}
	defer c.pruning.Store(false)
	if err := c.Prune(ctx); err != nil && c.logger != nil {
		c.logger.Warn("缓存清理失败", "err", err)
	}
}

// liveSize 返回缓存库的页级活跃大小（已用页减去 freelist 页）。
// 它包含索引与页开销，比按业务字段累加更接近真实占用，但既不含 freelist 页，
// 也不代表 cache.db 的磁盘占用。查询是 O(1) 的。
//
// 缓存使用单连接（SetMaxOpenConns(1)），任何未提交的事务都会一直占用它，
// 因此 liveSize 不得在任何 *sql.Tx 未提交时调用，否则会永久阻塞。
func (c *Cache) liveSize(ctx context.Context) (int64, error) {
	if c == nil {
		return 0, nil
	}
	var size int64
	err := c.db.QueryRowContext(ctx, `
		SELECT (page_count - freelist_count) * page_size
		FROM pragma_page_count, pragma_freelist_count, pragma_page_size`).Scan(&size)
	if err != nil {
		return 0, err
	}
	return size, nil
}

// Prune 删除过期条目，并按容量淘汰最久未使用的条目。
//
// 容量按页级活跃大小判断，超过 maxBytes 时按 last_used 批量删除，直到降至目标水位。
// 每批使用独立的隐式事务，提交后才重新测量；删除粒度是页，少量小行可能腾不出整页，
// 因此连续无进展时会提前停止。maybePrune 用 pruning 标志避免重入；
// 直接调用 Prune 的代码需自行保证串行。
func (c *Cache) Prune(ctx context.Context) error {
	if c == nil {
		return nil
	}
	cutoff := c.cutoff()
	if _, err := c.db.ExecContext(ctx, `
		DELETE FROM web_cache WHERE content_at < ? AND summary_at < ?`, cutoff, cutoff); err != nil {
		return err
	}

	live, err := c.liveSize(ctx)
	if err != nil {
		return err
	}
	if live <= c.maxBytes {
		return nil
	}

	target := c.maxBytes * pruneTargetPercent / 100
	var rows int64
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_cache`).Scan(&rows); err != nil {
		return err
	}
	noProgress := 0
	for round := 0; round < pruneMaxRounds && live > target && rows > 0; round++ {
		// 按需要释放的比例估算本轮删除行数：行大小均匀时一轮即可逼近目标；
		// 不均匀时由后续轮次修正。相比固定翻倍，接近上限时不会大幅过度淘汰。
		batch := rows * (live - target) / live
		if batch < 1 {
			batch = 1
		}
		if batch > pruneBatchMax {
			batch = pruneBatchMax
		}
		res, err := c.db.ExecContext(ctx, `
			DELETE FROM web_cache WHERE url IN (
				SELECT url FROM web_cache ORDER BY last_used, url LIMIT ?)`, batch)
		if err != nil {
			return err
		}
		deleted, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if deleted == 0 {
			break
		}
		rows -= deleted
		if rows < 0 {
			rows = 0
		}
		next, err := c.liveSize(ctx)
		if err != nil {
			return err
		}
		if next >= live {
			noProgress++
		} else {
			noProgress = 0
		}
		live = next
		if noProgress >= 2 {
			break
		}
	}
	if live > target && c.logger != nil {
		c.logger.Warn("缓存淘汰未达到目标水位",
			"live_bytes", live, "target_bytes", target, "max_bytes", c.maxBytes)
	}
	return nil
}

// cacheKey 去掉 fragment，并统一 scheme/host 大小写；path 与 query 保持原样。
func cacheKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String()
}
