package auth

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(StoreOptions{Path: filepath.Join(t.TempDir(), "auth.db")})
	if err != nil {
		t.Fatalf("打开访问密钥库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStoreAddListAuthenticateRemove(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	has, err := store.HasKeys(ctx)
	if err != nil {
		t.Fatalf("HasKeys 失败: %v", err)
	}
	if has {
		t.Fatal("空库不应有访问密钥")
	}

	key, plain, err := store.Add(ctx, "laptop")
	if err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if !strings.HasPrefix(plain, keyValuePrefix) {
		t.Fatalf("密钥前缀不符: %q", plain)
	}
	if len(plain) != len(keyValuePrefix)+43 {
		t.Fatalf("密钥长度不符: %d", len(plain))
	}
	if key.Prefix != plain[:keyDisplayPrefixLen] || key.Suffix != plain[len(plain)-keyDisplaySuffixLen:] {
		t.Fatalf("首尾片段不符: %#v vs %q", key, plain)
	}
	wantMasked := key.Prefix + "…" + key.Suffix
	if key.Masked() != wantMasked {
		t.Fatalf("Masked = %q, want %q", key.Masked(), wantMasked)
	}

	has, err = store.HasKeys(ctx)
	if err != nil || !has {
		t.Fatalf("HasKeys 应为 true，err=%v", err)
	}

	keys, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(keys) != 1 || keys[0].Name != "laptop" || keys[0].Masked() != wantMasked {
		t.Fatalf("List 结果不符: %#v", keys)
	}
	if strings.Contains(keys[0].Masked(), plain) {
		t.Fatal("脱敏结果不应包含完整密钥")
	}

	var keyHash, prefix, suffix string
	err = store.db.QueryRowContext(ctx,
		`SELECT key_hash, key_prefix, key_suffix FROM api_keys WHERE name = ?`, "laptop",
	).Scan(&keyHash, &prefix, &suffix)
	if err != nil {
		t.Fatalf("读取密钥记录失败: %v", err)
	}
	if keyHash == plain || strings.Contains(keyHash, plain) {
		t.Fatal("库里不应保存明文密钥")
	}
	if prefix != key.Prefix || suffix != key.Suffix {
		t.Fatalf("首尾片段写入不符: %q %q", prefix, suffix)
	}

	got, ok, err := store.Authenticate(ctx, plain)
	if err != nil || !ok {
		t.Fatalf("正确密钥应通过校验，ok=%v err=%v", ok, err)
	}
	if got.Name != "laptop" {
		t.Fatalf("校验返回的名称不符: %q", got.Name)
	}
	if _, ok, err := store.Authenticate(ctx, plain+"x"); err != nil || ok {
		t.Fatalf("错误密钥不应通过校验，ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Authenticate(ctx, strings.Repeat("a", maxKeyLen+1)); err != nil || ok {
		t.Fatalf("超长密钥不应通过校验，ok=%v err=%v", ok, err)
	}

	removedKey, removed, err := store.Remove(ctx, "laptop")
	if err != nil || !removed {
		t.Fatalf("Remove 失败: removed=%v err=%v", removed, err)
	}
	if removedKey.Name != "laptop" || removedKey.Masked() != wantMasked {
		t.Fatalf("Remove 返回的记录不符: %#v", removedKey)
	}
	if len(store.lastUsed) != 0 {
		t.Fatalf("Remove 后 lastUsed 应被清理: %#v", store.lastUsed)
	}
	removedKey, removed, err = store.Remove(ctx, "laptop")
	if err != nil || removed {
		t.Fatalf("重复 Remove 应返回 false: removed=%v err=%v", removed, err)
	}
	if removedKey.Name != "" {
		t.Fatalf("重复 Remove 不应返回记录: %#v", removedKey)
	}
	has, err = store.HasKeys(ctx)
	if err != nil || has {
		t.Fatalf("删除后 HasKeys 应为 false，err=%v", err)
	}
}

func TestStoreNameValidation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	bad := []string{"", "   ", strings.Repeat("a", maxNameRunes+1), "a\x00b"}
	for _, name := range bad {
		if _, _, err := store.Add(ctx, name); err == nil {
			t.Fatalf("名称 %q 应被拒绝", name)
		}
	}

	if _, _, err := store.Add(ctx, "my laptop"); err != nil {
		t.Fatalf("带空格的名称应被接受: %v", err)
	}
	if _, _, err := store.Add(ctx, "my laptop"); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("重名应报错: %v", err)
	}
}

func TestStoreLastUsedThrottle(t *testing.T) {
	store := openTestStore(t)
	base := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	now := base
	store.now = func() time.Time { return now }

	_, plain, err := store.Add(context.Background(), "k")
	if err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if _, ok, err := store.Authenticate(context.Background(), plain); err != nil || !ok {
		t.Fatalf("校验失败: ok=%v err=%v", ok, err)
	}
	first := listOne(t, store).LastUsedAt
	if !first.Equal(base) {
		t.Fatalf("首次使用时间 = %s, want %s", first, base)
	}

	now = base.Add(30 * time.Second)
	if _, ok, err := store.Authenticate(context.Background(), plain); err != nil || !ok {
		t.Fatalf("校验失败: ok=%v err=%v", ok, err)
	}
	if got := listOne(t, store).LastUsedAt; !got.Equal(first) {
		t.Fatalf("1 分钟内不应重复写 last_used_at: %s", got)
	}

	now = base.Add(61 * time.Second)
	if _, ok, err := store.Authenticate(context.Background(), plain); err != nil || !ok {
		t.Fatalf("校验失败: ok=%v err=%v", ok, err)
	}
	if got := listOne(t, store).LastUsedAt; !got.Equal(now) {
		t.Fatalf("超过 1 分钟后应更新 last_used_at: %s", got)
	}
}

func TestStoreUnavailable(t *testing.T) {
	store := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if _, err := store.HasKeys(context.Background()); err == nil {
		t.Fatal("关闭后 HasKeys 应报错")
	}
	if _, _, err := store.Add(context.Background(), "k"); err == nil {
		t.Fatal("关闭后 Add 应报错")
	}
	if _, err := store.List(context.Background()); err == nil {
		t.Fatal("关闭后 List 应报错")
	}
	if _, _, err := store.Authenticate(context.Background(), "k"); err == nil {
		t.Fatal("关闭后 Authenticate 应报错")
	}
}

func listOne(t *testing.T, store *Store) Key {
	t.Helper()
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("期望 1 条记录，实际 %d", len(keys))
	}
	return keys[0]
}
