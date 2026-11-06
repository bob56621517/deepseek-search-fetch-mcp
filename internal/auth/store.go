// Package auth 提供 MCP 入口的客户端访问密钥、鉴权中间件与限流。
// 这里的访问密钥只保护本服务的 /mcp 入口，与上游 deepseek_api_key 无关。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	// keyValuePrefix 是新生成访问密钥的固定前缀。
	keyValuePrefix = "dsf_"
	// keyRandomBytes 是随机部分的字节数，base64url 后为 43 个字符。
	keyRandomBytes = 32
	// keyDisplayPrefixLen / keyDisplaySuffixLen 仅用于列表首尾脱敏展示。
	keyDisplayPrefixLen = 12
	keyDisplaySuffixLen = 4
	// maxKeyLen 限制客户端提交的密钥长度，避免对超大字符串做哈希。
	maxKeyLen = 512
	// maxNameRunes 限制密钥名称长度。
	maxNameRunes = 64
	// lastUsedWriteInterval 限制 last_used_at 的写库频率。
	lastUsedWriteInterval = time.Minute
)

// StoreOptions 是访问密钥库的打开参数。
type StoreOptions struct {
	Path   string
	Logger *slog.Logger
}

// Key 是一条访问密钥记录。明文密钥不会保存在 Key 中。
type Key struct {
	ID         int64
	Name       string
	Prefix     string
	Suffix     string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// Masked 返回首尾脱敏后的展示形式，例如 dsf_Ab12Cd34…x9Yz。
func (k Key) Masked() string {
	if k.Suffix == "" {
		return k.Prefix + "…"
	}
	return k.Prefix + "…" + k.Suffix
}

type rowScanner interface {
	Scan(dest ...any) error
}

// scanKey 统一读取 id/name/key_prefix/key_suffix/created_at/last_used_at 六列。
func scanKey(row rowScanner) (Key, error) {
	var (
		key        Key
		createdAt  int64
		lastUsedAt int64
	)
	if err := row.Scan(&key.ID, &key.Name, &key.Prefix, &key.Suffix, &createdAt, &lastUsedAt); err != nil {
		return Key{}, err
	}
	if createdAt > 0 {
		key.CreatedAt = time.Unix(createdAt, 0).UTC()
	}
	if lastUsedAt > 0 {
		key.LastUsedAt = time.Unix(lastUsedAt, 0).UTC()
	}
	return key, nil
}

// Store 是落盘到 SQLite 的访问密钥库。
type Store struct {
	db     *sql.DB
	logger *slog.Logger
	// now 可在测试中替换。
	now func() time.Time

	lastUsedMu sync.Mutex
	lastUsed   map[int64]time.Time
}

// OpenStore 打开（必要时创建）访问密钥库。
func OpenStore(opts StoreOptions) (*Store, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("访问密钥库路径不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
		return nil, fmt.Errorf("创建访问密钥目录失败: %w", err)
	}

	dsn := filepath.ToSlash(opts.Path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开访问密钥库失败: %w", err)
	}
	// 鉴权查询很轻量，单连接足够，也能避免多连接之间的锁竞争。
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	store := &Store{
		db:       db,
		logger:   logger,
		now:      time.Now,
		lastUsed: make(map[int64]time.Time),
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接访问密钥库失败: %w", err)
	}
	if err := store.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(opts.Path, 0o600)
	return store, nil
}

func (s *Store) initSchema(ctx context.Context) error {
	const stmt = `CREATE TABLE IF NOT EXISTS api_keys (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		name         TEXT NOT NULL UNIQUE,
		key_hash     TEXT NOT NULL UNIQUE,
		key_prefix   TEXT NOT NULL,
		key_suffix   TEXT NOT NULL,
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0
	)`
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("初始化访问密钥表失败: %w", err)
	}
	return nil
}

// Close 关闭访问密钥库。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// HasKeys 报告是否存在至少一个访问密钥。
func (s *Store) HasKeys(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("访问密钥库未打开")
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM api_keys LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Add 生成并保存一条访问密钥，返回记录与只显示一次的明文密钥。
func (s *Store) Add(ctx context.Context, name string) (Key, string, error) {
	if s == nil || s.db == nil {
		return Key{}, "", errors.New("访问密钥库未打开")
	}
	name, err := normalizeName(name)
	if err != nil {
		return Key{}, "", err
	}

	var exists int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE name = ?`, name).Scan(&exists)
	if err == nil {
		return Key{}, "", fmt.Errorf("访问密钥名称已存在: %s", name)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Key{}, "", err
	}

	plain, err := generateKey()
	if err != nil {
		return Key{}, "", err
	}
	prefix, suffix := displayParts(plain)
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (name, key_hash, key_prefix, key_suffix, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, 0)`,
		name, hashKey(plain), prefix, suffix, now.Unix())
	if err != nil {
		// 两个进程同时 add 同名密钥时，预检查会漏过，这里再识别一次唯一约束。
		if strings.Contains(err.Error(), "UNIQUE constraint failed: api_keys.name") {
			return Key{}, "", fmt.Errorf("访问密钥名称已存在: %s", name)
		}
		return Key{}, "", fmt.Errorf("写入访问密钥失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		id = 0
	}
	return Key{
		ID:        id,
		Name:      name,
		Prefix:    prefix,
		Suffix:    suffix,
		CreatedAt: now,
	}, plain, nil
}

// List 返回全部访问密钥记录，按 name 排序。
func (s *Store) List(ctx context.Context) ([]Key, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("访问密钥库未打开")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, key_prefix, key_suffix, created_at, last_used_at
		FROM api_keys ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make([]Key, 0)
	for rows.Next() {
		key, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Remove 按名称硬删除一条访问密钥，并返回被删除的记录。
func (s *Store) Remove(ctx context.Context, name string) (Key, bool, error) {
	if s == nil || s.db == nil {
		return Key{}, false, errors.New("访问密钥库未打开")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Key{}, false, errors.New("访问密钥名称不能为空")
	}
	key, err := scanKey(s.db.QueryRowContext(ctx, `
		SELECT id, name, key_prefix, key_suffix, created_at, last_used_at
		FROM api_keys WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, key.ID)
	if err != nil {
		return Key{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Key{}, false, err
	}
	if n == 0 {
		return Key{}, false, nil
	}
	s.lastUsedMu.Lock()
	delete(s.lastUsed, key.ID)
	s.lastUsedMu.Unlock()
	return key, true, nil
}

// Authenticate 校验明文密钥，成功时顺带节流更新 last_used_at。
func (s *Store) Authenticate(ctx context.Context, plain string) (Key, bool, error) {
	if s == nil || s.db == nil {
		return Key{}, false, errors.New("访问密钥库未打开")
	}
	if plain == "" || len(plain) > maxKeyLen {
		return Key{}, false, nil
	}

	key, err := scanKey(s.db.QueryRowContext(ctx, `
		SELECT id, name, key_prefix, key_suffix, created_at, last_used_at
		FROM api_keys WHERE key_hash = ?`, hashKey(plain)))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, err
	}
	s.touchLastUsed(ctx, key.ID)
	return key, true, nil
}

// touchLastUsed 每个 key 最多每分钟写一次，失败只记日志。
func (s *Store) touchLastUsed(ctx context.Context, id int64) {
	if id == 0 {
		return
	}
	now := s.now().UTC()
	s.lastUsedMu.Lock()
	if last, ok := s.lastUsed[id]; ok && now.Sub(last) < lastUsedWriteInterval {
		s.lastUsedMu.Unlock()
		return
	}
	s.lastUsed[id] = now
	s.lastUsedMu.Unlock()

	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now.Unix(), id); err != nil {
		s.logger.Warn("更新访问密钥最后使用时间失败", "id", id, "err", err)
	}
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("访问密钥名称不能为空")
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		return "", fmt.Errorf("访问密钥名称不能超过 %d 个字符", maxNameRunes)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("访问密钥名称不能包含控制字符")
		}
	}
	return name, nil
}

func generateKey() (string, error) {
	raw := make([]byte, keyRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成访问密钥失败: %w", err)
	}
	return keyValuePrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func displayParts(plain string) (string, string) {
	if len(plain) <= keyDisplayPrefixLen+keyDisplaySuffixLen {
		return plain, ""
	}
	return plain[:keyDisplayPrefixLen], plain[len(plain)-keyDisplaySuffixLen:]
}

func hashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
