package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type middlewareTestEnv struct {
	middleware *Middleware
	store      *Store
	plain      string
	logs       *strings.Builder
}

func newMiddlewareTestEnv(t *testing.T, limit LimitConfig, withKey bool) *middlewareTestEnv {
	t.Helper()
	return newMiddlewareTestEnvWithResolver(t, limit, withKey, nil)
}

func newMiddlewareTestEnvWithResolver(t *testing.T, limit LimitConfig, withKey bool, resolver *ClientIPResolver) *middlewareTestEnv {
	t.Helper()
	store, err := OpenStore(StoreOptions{Path: filepath.Join(t.TempDir(), "auth.db")})
	if err != nil {
		t.Fatalf("打开访问密钥库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var plain string
	if withKey {
		_, plain, err = store.Add(context.Background(), "test")
		if err != nil {
			t.Fatalf("添加访问密钥失败: %v", err)
		}
	}

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if resolver == nil {
		resolver, err = NewClientIPResolver(nil)
		if err != nil {
			t.Fatalf("创建 IP 解析器失败: %v", err)
		}
	}
	middleware, err := NewMiddleware(MiddlewareOptions{
		Store:    store,
		Limiter:  NewLimiter(limit),
		Resolver: resolver,
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("创建中间件失败: %v", err)
	}
	return &middlewareTestEnv{middleware: middleware, store: store, plain: plain, logs: &logs}
}

func (e *middlewareTestEnv) request(t *testing.T, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return e.requestWith(t, headers, "", "")
}

func (e *middlewareTestEnv) requestWith(t *testing.T, headers map[string]string, remoteAddr, xff string) *httptest.ResponseRecorder {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	e.middleware.Wrap(next).ServeHTTP(rec, req)
	return rec
}

func TestMiddlewareOpenMode(t *testing.T) {
	env := newMiddlewareTestEnv(t, LimitConfig{}, false)
	if err := env.middleware.RefreshMode(context.Background()); err != nil {
		t.Fatalf("RefreshMode 失败: %v", err)
	}
	rec := env.request(t, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("没有访问密钥时应放行，状态码 %d", rec.Code)
	}
	if !strings.Contains(env.logs.String(), "访问鉴权未启用") || !strings.Contains(env.logs.String(), "mode=disabled") {
		t.Fatalf("应记录未启用日志: %s", env.logs.String())
	}
}

func TestMiddlewareAuthRequired(t *testing.T) {
	env := newMiddlewareTestEnv(t, LimitConfig{}, true)
	if err := env.middleware.RefreshMode(context.Background()); err != nil {
		t.Fatalf("RefreshMode 失败: %v", err)
	}

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"缺少凭据", nil, http.StatusUnauthorized},
		{"错误密钥", map[string]string{"Authorization": "Bearer wrong"}, http.StatusUnauthorized},
		{"Bearer", map[string]string{"Authorization": "Bearer " + env.plain}, http.StatusOK},
		{"X-API-Key", map[string]string{"X-API-Key": env.plain}, http.StatusOK},
		{"两种相同凭据", map[string]string{"Authorization": "Bearer " + env.plain, "X-API-Key": env.plain}, http.StatusOK},
		{"两种不同凭据", map[string]string{"Authorization": "Bearer " + env.plain, "X-API-Key": "other"}, http.StatusUnauthorized},
		{"非 Bearer", map[string]string{"Authorization": "Basic abc"}, http.StatusUnauthorized},
		{"Bearer 缺少值", map[string]string{"Authorization": "Bearer"}, http.StatusUnauthorized},
		{"超长密钥", map[string]string{"X-API-Key": strings.Repeat("a", maxKeyLen+1)}, http.StatusUnauthorized},
	}
	first401Body := ""
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.request(t, tc.headers)
			if rec.Code != tc.want {
				t.Fatalf("状态码 = %d, want %d, body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusUnauthorized {
				return
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 应带 WWW-Authenticate")
			}
			body := rec.Body.String()
			if first401Body == "" {
				first401Body = body
			} else if body != first401Body {
				t.Fatalf("401 响应体应一致: %q != %q", body, first401Body)
			}
			if strings.Contains(body, "reason") || strings.Contains(body, env.plain) {
				t.Fatalf("401 响应体不应包含 reason 或密钥: %s", body)
			}
		})
	}

	logs := env.logs.String()
	if !strings.Contains(logs, "event=auth_fail") || !strings.Contains(logs, "status=401") {
		t.Fatalf("鉴权失败日志字段不符: %s", logs)
	}
	if !strings.Contains(logs, "remote=192.0.2.1") {
		t.Fatalf("日志应包含客户端 IP: %s", logs)
	}
	if strings.Contains(logs, env.plain) {
		t.Fatal("日志不应包含完整访问密钥")
	}
}

func TestMiddlewareAuthFailureBlock(t *testing.T) {
	env := newMiddlewareTestEnv(t, LimitConfig{
		AuthFailLimit:  2,
		AuthFailWindow: time.Minute,
		AuthFailBlock:  5 * time.Minute,
	}, true)
	if err := env.middleware.RefreshMode(context.Background()); err != nil {
		t.Fatalf("RefreshMode 失败: %v", err)
	}

	for i := 0; i < 2; i++ {
		if rec := env.request(t, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应返回 401，实际 %d", i+1, rec.Code)
		}
	}
	rec := env.request(t, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超过失败阈值应返回 429，实际 %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}
	if !strings.Contains(env.logs.String(), "event=auth_blocked") {
		t.Fatalf("应记录 auth_blocked: %s", env.logs.String())
	}
}

func TestMiddlewareKeyRateLimit(t *testing.T) {
	env := newMiddlewareTestEnv(t, LimitConfig{KeyRPS: 1, KeyBurst: 1}, true)
	if err := env.middleware.RefreshMode(context.Background()); err != nil {
		t.Fatalf("RefreshMode 失败: %v", err)
	}

	headers := map[string]string{"Authorization": "Bearer " + env.plain}
	if rec := env.request(t, headers); rec.Code != http.StatusOK {
		t.Fatalf("首个请求应放行，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec := env.request(t, headers)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第二个请求应被限流，实际 %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}
	if !strings.Contains(env.logs.String(), "event=key_rate_limited") || !strings.Contains(env.logs.String(), "key_name=test") {
		t.Fatalf("应记录 key_rate_limited: %s", env.logs.String())
	}
}

func TestMiddlewareIPRateLimit(t *testing.T) {
	env := newMiddlewareTestEnv(t, LimitConfig{IPRPS: 1, IPBurst: 1}, true)
	if err := env.middleware.RefreshMode(context.Background()); err != nil {
		t.Fatalf("RefreshMode 失败: %v", err)
	}

	headers := map[string]string{"Authorization": "Bearer " + env.plain}
	if rec := env.request(t, headers); rec.Code != http.StatusOK {
		t.Fatalf("首个请求应放行，实际 %d", rec.Code)
	}
	rec := env.request(t, headers)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第二个请求应被 IP 限流，实际 %d", rec.Code)
	}
	if !strings.Contains(env.logs.String(), "event=ip_rate_limited") {
		t.Fatalf("应记录 ip_rate_limited: %s", env.logs.String())
	}
}

func TestMiddlewareStoreUnavailable(t *testing.T) {
	env := newMiddlewareTestEnv(t, LimitConfig{}, true)
	if err := env.store.Close(); err != nil {
		t.Fatalf("关闭访问密钥库失败: %v", err)
	}
	rec := env.request(t, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("鉴权库不可用应返回 503，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "auth_unavailable") {
		t.Fatalf("响应体不符: %s", rec.Body.String())
	}
	logs := env.logs.String()
	if !strings.Contains(logs, "event=auth_unavailable") || !strings.Contains(logs, "status=503") {
		t.Fatalf("应记录 auth_unavailable: %s", logs)
	}
}

func TestMiddlewareModeTransition(t *testing.T) {
	env := newMiddlewareTestEnv(t, LimitConfig{}, false)
	if err := env.middleware.RefreshMode(context.Background()); err != nil {
		t.Fatalf("RefreshMode 失败: %v", err)
	}
	if rec := env.request(t, nil); rec.Code != http.StatusOK {
		t.Fatalf("无密钥时应放行，实际 %d", rec.Code)
	}

	if _, _, err := env.store.Add(context.Background(), "later"); err != nil {
		t.Fatalf("添加密钥失败: %v", err)
	}
	if rec := env.request(t, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("添加密钥后应强制鉴权，实际 %d", rec.Code)
	}
	if !strings.Contains(env.logs.String(), "访问鉴权已启用") || !strings.Contains(env.logs.String(), "mode=enforced") {
		t.Fatalf("应记录启用日志: %s", env.logs.String())
	}

	_, removed, err := env.store.Remove(context.Background(), "later")
	if err != nil || !removed {
		t.Fatalf("删除密钥失败: removed=%v err=%v", removed, err)
	}
	if rec := env.request(t, nil); rec.Code != http.StatusOK {
		t.Fatalf("删除最后一个密钥后应放行，实际 %d", rec.Code)
	}
	if !strings.Contains(env.logs.String(), "访问鉴权已关闭") || !strings.Contains(env.logs.String(), "mode=disabled") {
		t.Fatalf("应记录关闭日志: %s", env.logs.String())
	}
}

func TestMiddlewareRefreshModeError(t *testing.T) {
	env := newMiddlewareTestEnv(t, LimitConfig{}, true)
	if err := env.store.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if err := env.middleware.RefreshMode(context.Background()); err == nil {
		t.Fatal("访问密钥库关闭后 RefreshMode 应报错")
	}
}

func TestMiddlewareAuthFailureUsesForwardedClient(t *testing.T) {
	resolver, err := NewClientIPResolver([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("创建 IP 解析器失败: %v", err)
	}
	env := newMiddlewareTestEnvWithResolver(t, LimitConfig{
		AuthFailLimit:  2,
		AuthFailWindow: time.Minute,
		AuthFailBlock:  5 * time.Minute,
	}, true, resolver)

	// 非法项在左侧时，仍应取到右侧真实客户端，失败计数不能落到代理 IP。
	for i := 0; i < 2; i++ {
		rec := env.requestWith(t, nil, "127.0.0.1:1234", "garbage, 203.0.113.5")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应返回 401，实际 %d", i+1, rec.Code)
		}
	}
	if blocked, _ := env.middleware.limiter.AuthBlocked("127.0.0.1"); blocked {
		t.Fatal("失败计数不应落到代理 IP")
	}
	if blocked, _ := env.middleware.limiter.AuthBlocked("203.0.113.5"); !blocked {
		t.Fatal("失败计数应落到真实客户端 IP")
	}
	if !strings.Contains(env.logs.String(), "remote=203.0.113.5") {
		t.Fatalf("日志应记录真实客户端 IP: %s", env.logs.String())
	}
}

func TestNewMiddlewareRequiresLimiter(t *testing.T) {
	store := openTestStore(t)
	if _, err := NewMiddleware(MiddlewareOptions{Store: store}); err == nil {
		t.Fatal("缺少 Limiter 应报错")
	}
}
