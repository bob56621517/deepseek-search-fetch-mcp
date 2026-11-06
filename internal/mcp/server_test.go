package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bob56621517/deepseek-search-fetch-mcp/internal/auth"
	mcpserver "github.com/bob56621517/deepseek-search-fetch-mcp/internal/mcp"
	"github.com/bob56621517/deepseek-search-fetch-mcp/internal/openapi"
)

type upstreamRequest struct {
	path   string
	header http.Header
	body   []byte
}

type fakeUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	status int
	body   string
	reqs   []upstreamRequest
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{status: http.StatusOK, body: `{}`}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, upstreamRequest{path: r.URL.Path, header: r.Header.Clone(), body: body})
		status, respBody := f.status, f.body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUpstream) respond(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
	f.body = body
}

func (f *fakeUpstream) calls() []upstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]upstreamRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

type testEnv struct {
	upstream *fakeUpstream
	http     *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	up := newFakeUpstream(t)
	client := openapi.NewClient(up.server.URL, "test-key", openapi.DefaultOptions())
	srv := mcpserver.New(mcpserver.Options{
		Client:  client,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &testEnv{upstream: up, http: hs}
}

func (e *testEnv) connect(t *testing.T) (*sdkmcp.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint:             e.http.URL + "/mcp",
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		cancel()
		t.Fatalf("连接服务失败: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
	})
	return session, ctx
}

func resultText(t *testing.T, res *sdkmcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("期望一条 content: %#v", res)
	}
	text, ok := res.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("期望 text content，实际 %T", res.Content[0])
	}
	return text.Text
}

func TestListTools(t *testing.T) {
	env := newTestEnv(t)
	session, ctx := env.connect(t)

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list 失败: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"web_ext_fetch", "web_ext_search"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("工具列表不符\n got: %v\nwant: %v", names, want)
	}
}

func TestCallSearch(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.respond(200, `{"results":[{"title":"go","url":"https://go.dev","snippet":"short","summary":"long","rerank_score":2.5,"authority":3}]}`)
	session, ctx := env.connect(t)

	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "web_ext_search",
		Arguments: map[string]any{"query": "golang"},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("不应是错误结果: %s", resultText(t, res))
	}
	var got struct {
		Results []struct {
			Title       string  `json:"title"`
			URL         string  `json:"url"`
			Snippet     string  `json:"snippet"`
			Summary     string  `json:"summary"`
			RerankScore float64 `json:"rerank_score"`
			Authority   int     `json:"authority"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if len(got.Results) != 1 {
		t.Fatalf("结果数量不符: %d", len(got.Results))
	}
	if r := got.Results[0]; r.Snippet != "short" || r.Summary != "" || r.RerankScore != 2.5 || r.Authority != 3 {
		t.Fatalf("默认搜索结果不符: %#v", r)
	}

	calls := env.upstream.calls()
	if len(calls) != 1 {
		t.Fatalf("上游调用次数不符: %d", len(calls))
	}
	if calls[0].path != "/v1/tools/search" {
		t.Fatalf("请求路径不符: %s", calls[0].path)
	}
	if got := calls[0].header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("鉴权头不符: %q", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(calls[0].body, &payload); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if payload["query"] != "golang" || payload["count"] != float64(10) {
		t.Fatalf("请求体不符: %#v", payload)
	}
	if _, ok := payload["summary"]; ok {
		t.Fatalf("summary 是包装层参数，不应发往上游: %#v", payload)
	}
}

func TestCallToolError(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.respond(500, `{"error":{"message":"服务器内部错误","type":"server_error","code":"server_error"}}`)
	session, ctx := env.connect(t)

	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "web_ext_fetch",
		Arguments: map[string]any{"url": "https://example.com/a"},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !res.IsError {
		t.Fatal("应返回 isError")
	}
	text := resultText(t, res)
	if !strings.Contains(text, "HTTP 500") || !strings.Contains(text, "服务器内部错误") {
		t.Fatalf("错误信息不符: %s", text)
	}
}

func TestInvalidArguments(t *testing.T) {
	env := newTestEnv(t)
	session, ctx := env.connect(t)

	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "web_ext_search",
		Arguments: map[string]any{"query": "   "},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !res.IsError {
		t.Fatal("参数非法时应返回 isError")
	}
	if len(env.upstream.calls()) != 0 {
		t.Fatal("参数校验失败时不应请求上游")
	}
}

func TestHealth(t *testing.T) {
	env := newTestEnv(t)
	for _, path := range []string{"/health", "/healthz"} {
		resp, err := http.Get(env.http.URL + path)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态码应为 200，实际 %d", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), `"status":"ok"`) {
			t.Fatalf("%s 响应不符: %s", path, body)
		}
	}
}

func TestAccessKeyAuth(t *testing.T) {
	up := newFakeUpstream(t)
	client := openapi.NewClient(up.server.URL, "test-key", openapi.DefaultOptions())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := auth.OpenStore(auth.StoreOptions{Path: filepath.Join(t.TempDir(), "auth.db")})
	if err != nil {
		t.Fatalf("打开访问密钥库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, plain, err := store.Add(context.Background(), "test")
	if err != nil {
		t.Fatalf("添加访问密钥失败: %v", err)
	}
	resolver, err := auth.NewClientIPResolver(nil)
	if err != nil {
		t.Fatalf("创建 IP 解析器失败: %v", err)
	}
	middleware, err := auth.NewMiddleware(auth.MiddlewareOptions{
		Store:    store,
		Limiter:  auth.NewLimiter(auth.LimitConfig{}),
		Resolver: resolver,
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("创建鉴权中间件失败: %v", err)
	}
	if err := middleware.RefreshMode(context.Background()); err != nil {
		t.Fatalf("读取鉴权模式失败: %v", err)
	}

	srv := mcpserver.New(mcpserver.Options{
		Client:  client,
		Logger:  logger,
		Version: "test",
		Auth:    middleware,
	})
	handler := srv.Handler()

	// 健康检查永远豁免鉴权。
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/health 应豁免鉴权，实际 %d", rec.Code)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"v0"}}}`
	newRequest := func(headers map[string]string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		return req
	}

	// 有密钥但请求未携带凭据时应拒绝。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, newRequest(nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺少凭据应返回 401，实际 %d", rec.Code)
	}

	// 有效凭据应穿透到 MCP 处理器；具体状态码取决于请求体是否完整，
	// 这里只断言不再被鉴权层拦截。
	for name, headers := range map[string]map[string]string{
		"Bearer":    {"Authorization": "Bearer " + plain},
		"X-API-Key": {"X-API-Key": plain},
	} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, newRequest(headers))
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s 有效凭据不应被拒绝: %s", name, rec.Body.String())
		}
	}
}
