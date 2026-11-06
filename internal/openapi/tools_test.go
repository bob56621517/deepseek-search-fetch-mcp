package openapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedRequest struct {
	path   string
	header http.Header
	body   []byte
}

type fakeUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	status int
	body   string
	reqs   []capturedRequest
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{status: http.StatusOK, body: `{}`}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, capturedRequest{path: r.URL.Path, header: r.Header.Clone(), body: body})
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

func (f *fakeUpstream) calls() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

func newTools(t *testing.T) ([]Tool, *fakeUpstream, *Cache) {
	t.Helper()
	return newToolsWithLang(t, "")
}

func newToolsWithLang(t *testing.T, defaultLang string) ([]Tool, *fakeUpstream, *Cache) {
	t.Helper()
	up := newFakeUpstream(t)
	cache, err := OpenCache(CacheOptions{
		Path: filepath.Join(t.TempDir(), "cache.db"),
		TTL:  time.Minute,
	})
	if err != nil {
		t.Fatalf("打开缓存失败: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tools := Tools(NewClient(up.server.URL, "test-key", DefaultOptions()), cache, logger, defaultLang)
	return tools, up, cache
}

func findTool(t *testing.T, all []Tool, name string) Tool {
	t.Helper()
	for _, tool := range all {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("未找到工具 %s", name)
	return Tool{}
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v (%s)", err, body)
	}
	return out
}

func TestToolDefinitions(t *testing.T) {
	all, _, _ := newTools(t)
	if len(all) != 2 {
		t.Fatalf("工具数量应为 2，实际 %d", len(all))
	}
	search := findTool(t, all, "web_ext_search")
	fetch := findTool(t, all, "web_ext_fetch")
	if search.Description == "" || fetch.Description == "" {
		t.Fatal("工具描述不能为空")
	}
	if search.InputSchema["required"].([]string)[0] != "query" {
		t.Fatal("search 必须要求 query")
	}
	if fetch.InputSchema["required"].([]string)[0] != "url" {
		t.Fatal("fetch 必须要求 url")
	}
	searchProps := search.InputSchema["properties"].(map[string]any)
	if _, ok := searchProps["summary"]; ok {
		t.Fatal("search 不应再暴露 summary 参数")
	}
	fetchProps := fetch.InputSchema["properties"].(map[string]any)
	for _, key := range []string{"summary", "offset", "limit", "refresh"} {
		if _, ok := fetchProps[key]; !ok {
			t.Fatalf("fetch 缺少参数 %s", key)
		}
	}
	limitSchema := fetchProps["limit"].(map[string]any)
	if limitSchema["minimum"] != 0 {
		t.Fatalf("limit 应允许 0（视为 8000），实际 schema: %#v", limitSchema)
	}
}

func TestSearchSuccess(t *testing.T) {
	all, up, cache := newTools(t)
	up.respond(200, `{"results":[{"title":"go","url":"https://go.dev","snippet":"short","summary":"long summary","content":"full content","site_icon":"https://icon","site_name":"Go","date":"2026-01-01T00:00:00Z","rerank_score":2.5,"authority":3}]}`)

	body, err := findTool(t, all, "web_ext_search").Call(
		context.Background(),
		json.RawMessage(`{"query":"golang","count":5,"ori_question":"what is go","localization":"1.2.3.4"}`),
	)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var resp searchOutput
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, body)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("结果数量不符: %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r.Snippet != "short" || r.RerankScore != 2.5 || r.Authority != 3 {
		t.Fatalf("搜索结果不符: %#v", r)
	}
	if r.SiteName == nil || *r.SiteName != "Go" {
		t.Fatalf("site_name 未保留: %#v", r)
	}
	if r.Date == nil || *r.Date != "2026-01-01T00:00:00Z" {
		t.Fatalf("date 未保留: %#v", r)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	result := raw["results"].([]any)[0].(map[string]any)
	for _, key := range []string{"content", "site_icon", "summary"} {
		if _, ok := result[key]; ok {
			t.Fatalf("响应不应包含 %s: %s", key, body)
		}
	}

	calls := up.calls()
	if len(calls) != 1 {
		t.Fatalf("上游调用次数不符: %d", len(calls))
	}
	if calls[0].path != "/v1/tools/search" {
		t.Fatalf("请求路径不符: %s", calls[0].path)
	}
	if got := calls[0].header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("鉴权头不符: %q", got)
	}
	payload := decodeBody(t, calls[0].body)
	if payload["query"] != "golang" || payload["count"] != float64(5) {
		t.Fatalf("请求体不符: %#v", payload)
	}
	if payload["need_content"] != true {
		t.Fatalf("开启缓存时应请求 need_content: %#v", payload)
	}
	if _, ok := payload["summary"]; ok {
		t.Fatalf("summary 参数已移除，不应发往上游: %#v", payload)
	}

	cachedSummary, ok, err := cache.GetSummary(context.Background(), "https://go.dev")
	if err != nil || !ok || cachedSummary.Summary != "long summary" {
		t.Fatalf("摘要未写入缓存: %#v err=%v", cachedSummary, err)
	}
	cachedContent, ok, err := cache.GetContent(context.Background(), "https://go.dev")
	if err != nil || !ok || cachedContent.Content != "full content" || cachedContent.ContentLength != 12 {
		t.Fatalf("正文未写入缓存: %#v err=%v", cachedContent, err)
	}
}

func TestSearchDefaults(t *testing.T) {
	all, up, _ := newTools(t)
	up.respond(200, `{"results":[]}`)

	if _, err := findTool(t, all, "web_ext_search").Call(context.Background(), json.RawMessage(`{"query":"q"}`)); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	payload := decodeBody(t, up.calls()[0].body)
	if payload["count"] != float64(10) {
		t.Fatalf("默认 count 应为 10: %#v", payload)
	}
	for _, key := range []string{"ori_question", "localization", "summary"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("未提供的 %s 不应出现在请求体中: %#v", key, payload)
		}
	}
	if payload["need_content"] != true {
		t.Fatalf("默认应请求正文以预热缓存: %#v", payload)
	}
}

func TestSearchCountClamped(t *testing.T) {
	cases := []struct {
		name string
		args string
		want float64
	}{
		{"过小值按 5 处理", `{"query":"q","count":3}`, 5},
		{"零值使用默认值", `{"query":"q","count":0}`, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			all, up, _ := newTools(t)
			up.respond(200, `{"results":[]}`)

			if _, err := findTool(t, all, "web_ext_search").Call(context.Background(), json.RawMessage(tc.args)); err != nil {
				t.Fatalf("调用失败: %v", err)
			}
			payload := decodeBody(t, up.calls()[0].body)
			if payload["count"] != tc.want {
				t.Fatalf("count 应为 %v，实际 %#v", tc.want, payload["count"])
			}
		})
	}
}

func TestSearchValidation(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"空 query", `{"query":"   "}`},
		{"query 超长", fmt.Sprintf(`{"query":%q}`, strings.Repeat("中", 129))},
		{"count 过大", `{"query":"q","count":51}`},
		{"count 为负", `{"query":"q","count":-1}`},
		{"ori_question 为空白", `{"query":"q","ori_question":"  "}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			all, up, _ := newTools(t)
			_, err := findTool(t, all, "web_ext_search").Call(context.Background(), json.RawMessage(tc.args))
			if err == nil {
				t.Fatal("应返回参数错误")
			}
			if len(up.calls()) != 0 {
				t.Fatal("参数校验失败时不应请求上游")
			}
		})
	}
}

func TestSearchAPIError(t *testing.T) {
	all, up, _ := newTools(t)
	up.respond(401, `{"error":{"message":"API Key 缺失或无效","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`)

	_, err := findTool(t, all, "web_ext_search").Call(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err == nil {
		t.Fatal("应返回上游错误")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误类型应为 *APIError，实际 %T", err)
	}
	if apiErr.StatusCode != 401 || apiErr.Message != "API Key 缺失或无效" {
		t.Fatalf("错误解析不符: %#v", apiErr)
	}
}

func TestFetchSuccessAndCache(t *testing.T) {
	all, up, _ := newTools(t)
	up.respond(200, `{"title":"Example","content":"# Example"}`)
	tool := findTool(t, all, "web_ext_fetch")

	body, err := tool.Call(context.Background(), json.RawMessage(`{"url":"https://example.com/article"}`))
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var first contentOutput
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, body)
	}
	if first.Kind != "content" || first.Content != "# Example" ||
		first.ContentLength != 9 || first.Length != 9 ||
		first.Offset != 0 || first.HasMore || first.HasSummary {
		t.Fatalf("首次抓取结果不符: %#v", first)
	}
	if first.Source != "upstream" || first.CachedAt == "" {
		t.Fatalf("首次抓取来源不符: %#v", first)
	}

	body, err = tool.Call(context.Background(), json.RawMessage(`{"url":"https://example.com/article"}`))
	if err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	var second contentOutput
	if err := json.Unmarshal(body, &second); err != nil {
		t.Fatal(err)
	}
	if second.Source != "cache" || second.Content != "# Example" {
		t.Fatalf("第二次应命中缓存: %#v", second)
	}
	if len(up.calls()) != 1 {
		t.Fatalf("命中缓存后不应再请求上游: %d", len(up.calls()))
	}
}

func TestFetchPagination(t *testing.T) {
	all, up, _ := newTools(t)
	up.respond(200, `{"title":"中文","content":"一二三四五六七八九十"}`)
	tool := findTool(t, all, "web_ext_fetch")

	call := func(args string) contentOutput {
		t.Helper()
		body, err := tool.Call(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("调用失败: %v", err)
		}
		var out contentOutput
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("结果不是合法 JSON: %v (%s)", err, body)
		}
		return out
	}

	first := call(`{"url":"https://example.com/zh","offset":2,"limit":3}`)
	if first.Content != "三四五" || first.Offset != 2 || first.Length != 3 ||
		first.ContentLength != 10 || !first.HasMore || first.NextOffset == nil || *first.NextOffset != 5 {
		t.Fatalf("分页结果不符: %#v", first)
	}
	last := call(`{"url":"https://example.com/zh","offset":8,"limit":3}`)
	if last.Content != "九十" || last.Length != 2 || last.HasMore || last.NextOffset != nil {
		t.Fatalf("末页结果不符: %#v", last)
	}
	beyond := call(`{"url":"https://example.com/zh","offset":99,"limit":3}`)
	if beyond.Content != "" || beyond.Length != 0 || beyond.HasMore {
		t.Fatalf("越界结果不符: %#v", beyond)
	}
	if len(up.calls()) != 1 {
		t.Fatalf("分页应复用缓存: %d", len(up.calls()))
	}
}

func TestFetchFullContentForCLI(t *testing.T) {
	_, up, cache := newTools(t)
	long := strings.Repeat("中", 20000)
	body, err := json.Marshal(fetchResponse{Title: "T", Content: long})
	if err != nil {
		t.Fatal(err)
	}
	up.respond(200, string(body))

	client := NewClient(up.server.URL, "test-key", DefaultOptions())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	out, err := FetchPage(context.Background(), client, cache, logger, FetchOptions{
		URL:         "https://example.com/long",
		FullContent: true,
	})
	if err != nil {
		t.Fatalf("完整抓取失败: %v", err)
	}
	var full contentOutput
	if err := json.Unmarshal(out, &full); err != nil {
		t.Fatal(err)
	}
	if full.Content != long || full.Length != 20000 || full.ContentLength != 20000 || full.HasMore {
		t.Fatalf("CLI 应返回完整正文: length=%d total=%d hasMore=%v", full.Length, full.ContentLength, full.HasMore)
	}

	// MCP 工具仍然受分页上限约束。
	all := Tools(NewClient(up.server.URL, "test-key", DefaultOptions()), cache, logger, "")
	toolBody, err := findTool(t, all, "web_ext_fetch").Call(
		context.Background(),
		json.RawMessage(`{"url":"https://example.com/long"}`),
	)
	if err != nil {
		t.Fatalf("MCP 抓取失败: %v", err)
	}
	var paged contentOutput
	if err := json.Unmarshal(toolBody, &paged); err != nil {
		t.Fatal(err)
	}
	if paged.Length != defaultFetchLimit || !paged.HasMore {
		t.Fatalf("MCP 工具应分页: %#v", paged)
	}
}

func TestFetchSummaryFromSearchCache(t *testing.T) {
	all, up, _ := newTools(t)
	up.respond(200, `{"results":[{"title":"Example","url":"https://example.com/a","snippet":"s","summary":"the summary","content":"body text","rerank_score":2,"authority":3}]}`)

	if _, err := findTool(t, all, "web_ext_search").Call(context.Background(), json.RawMessage(`{"query":"q"}`)); err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(up.calls()) != 1 {
		t.Fatalf("搜索调用次数不符: %d", len(up.calls()))
	}

	body, err := findTool(t, all, "web_ext_fetch").Call(
		context.Background(),
		json.RawMessage(`{"url":"https://example.com/a","summary":true,"offset":5,"limit":1}`),
	)
	if err != nil {
		t.Fatalf("读取摘要失败: %v", err)
	}
	var out summaryOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, body)
	}
	if out.Kind != "summary" || out.Summary != "the summary" || !out.HasSummary {
		t.Fatalf("摘要结果不符: %#v", out)
	}
	if out.ContentLength != 9 || out.Offset != 0 || out.Length != 11 || out.Source != "cache" {
		t.Fatalf("摘要元数据不符: %#v", out)
	}
	if out.SummaryQuery != "q" {
		t.Fatalf("摘要来源 query 不符: %#v", out)
	}
	if len(up.calls()) != 1 {
		t.Fatalf("摘要命中缓存后不应请求上游: %d", len(up.calls()))
	}
}

func TestFetchSummaryFallback(t *testing.T) {
	all, up, _ := newTools(t)
	up.respond(200, `{"title":"T","content":"abcdef"}`)

	body, err := findTool(t, all, "web_ext_fetch").Call(
		context.Background(),
		json.RawMessage(`{"url":"https://example.com/b","summary":true}`),
	)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var out contentOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, body)
	}
	if out.Kind != "content" || out.Content != "abcdef" || out.HasSummary || out.Hint == "" {
		t.Fatalf("摘要缺失时应回退为正文: %#v", out)
	}
	if out.Source != "upstream" || out.ContentLength != 6 {
		t.Fatalf("回退结果元数据不符: %#v", out)
	}
	if len(up.calls()) != 1 {
		t.Fatalf("回退时应抓取一次上游: %d", len(up.calls()))
	}
}

func TestFetchRefresh(t *testing.T) {
	all, up, cache := newTools(t)
	up.respond(200, `{"results":[{"title":"T","url":"https://example.com/c","snippet":"s","summary":"old summary","content":"old content","rerank_score":2,"authority":3}]}`)
	if _, err := findTool(t, all, "web_ext_search").Call(context.Background(), json.RawMessage(`{"query":"q"}`)); err != nil {
		t.Fatalf("搜索失败: %v", err)
	}

	up.respond(200, `{"title":"T","content":"new content"}`)
	body, err := findTool(t, all, "web_ext_fetch").Call(
		context.Background(),
		json.RawMessage(`{"url":"https://example.com/c","summary":true,"refresh":true}`),
	)
	if err != nil {
		t.Fatalf("强制刷新失败: %v", err)
	}
	var out contentOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "new content" || out.HasSummary || out.Source != "upstream" {
		t.Fatalf("强制刷新结果不符: %#v", out)
	}
	if _, ok, err := cache.GetSummary(context.Background(), "https://example.com/c"); err != nil || ok {
		t.Fatalf("强制刷新后旧摘要应被清除: ok=%v err=%v", ok, err)
	}
	if len(up.calls()) != 2 {
		t.Fatalf("应执行一次搜索和一次抓取: %d", len(up.calls()))
	}
}

func TestFetchOutputURLNormalized(t *testing.T) {
	all, up, _ := newTools(t)
	up.respond(200, `{"title":"T","content":"body"}`)
	tool := findTool(t, all, "web_ext_fetch")
	ctx := context.Background()
	const requestURL = "https://EXAMPLE.com/A#Frag"

	call := func() contentOutput {
		t.Helper()
		body, err := tool.Call(ctx, json.RawMessage(fmt.Sprintf(`{"url":%q}`, requestURL)))
		if err != nil {
			t.Fatalf("调用失败: %v", err)
		}
		var out contentOutput
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("结果不是合法 JSON: %v (%s)", err, body)
		}
		return out
	}

	first := call()
	if first.URL != "https://example.com/A" || first.Source != "upstream" {
		t.Fatalf("上游结果应输出规范化 URL: %#v", first)
	}
	second := call()
	if second.URL != first.URL || second.Source != "cache" {
		t.Fatalf("缓存命中的 URL 应与上游一致: %#v", second)
	}
}

func TestFetchRefreshEmptyContentClearsSummary(t *testing.T) {
	all, up, cache := newTools(t)
	ctx := context.Background()
	const url = "https://example.com/empty-refresh"

	if err := cache.PutSearch(ctx, "q", []CachedPage{{
		URL: url, Title: "T", Content: "old body", Summary: "old summary",
	}}); err != nil {
		t.Fatalf("预热缓存失败: %v", err)
	}

	up.respond(200, `{"title":"T","content":""}`)
	body, err := findTool(t, all, "web_ext_fetch").Call(
		ctx, json.RawMessage(fmt.Sprintf(`{"url":%q,"refresh":true}`, url)),
	)
	if err != nil {
		t.Fatalf("强制刷新失败: %v", err)
	}
	var out contentOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, body)
	}
	if out.Content != "" || out.HasSummary || out.Source != "upstream" {
		t.Fatalf("空正文刷新结果不符: %#v", out)
	}
	if _, ok, err := cache.GetSummary(ctx, url); err != nil || ok {
		t.Fatalf("空正文刷新后旧摘要应被清除: ok=%v err=%v", ok, err)
	}
	if page, ok, err := cache.GetContent(ctx, url); err != nil || !ok || page.Content != "old body" {
		t.Fatalf("空正文刷新不应写入新正文，旧正文应保留: ok=%v err=%v %#v", ok, err, page)
	}
}

func TestFormatTimeZero(t *testing.T) {
	if got := formatTime(time.Unix(0, 0)); got != "" {
		t.Fatalf("Unix 0 应输出空字符串，实际 %q", got)
	}
}

func TestFetchSummaryTooLongFallsBackToContent(t *testing.T) {
	all, up, cache := newTools(t)
	ctx := context.Background()
	const url = "https://example.com/long-summary"

	if err := cache.PutSearch(ctx, "q", []CachedPage{{
		URL: url, Title: "T", Content: "body", Summary: strings.Repeat("中", maxUsableSummaryRunes+1),
	}}); err != nil {
		t.Fatalf("预热缓存失败: %v", err)
	}

	body, err := findTool(t, all, "web_ext_fetch").Call(
		ctx, json.RawMessage(fmt.Sprintf(`{"url":%q,"summary":true}`, url)),
	)
	if err != nil {
		t.Fatalf("读取摘要失败: %v", err)
	}
	var out contentOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, body)
	}
	if out.Kind != "content" || out.Content != "body" || out.HasSummary || out.Hint == "" {
		t.Fatalf("超长摘要应按未命中处理并回退正文: %#v", out)
	}
	if out.Source != "cache" {
		t.Fatalf("正文应命中缓存: %#v", out)
	}
	if len(up.calls()) != 0 {
		t.Fatalf("回退正文时不应请求上游: %d", len(up.calls()))
	}

	// summary=false 时，超长摘要也不应被标记为可用。
	body, err = findTool(t, all, "web_ext_fetch").Call(
		ctx, json.RawMessage(fmt.Sprintf(`{"url":%q}`, url)),
	)
	if err != nil {
		t.Fatalf("读取正文失败: %v", err)
	}
	var content contentOutput
	if err := json.Unmarshal(body, &content); err != nil {
		t.Fatal(err)
	}
	if content.HasSummary {
		t.Fatalf("超长摘要不应标记 hasSummary: %#v", content)
	}
}

func TestFetchValidation(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"空 url", `{}`},
		{"非 HTTP", `{"url":"ftp://example.com/a"}`},
		{"缺少主机名", `{"url":"https:///a"}`},
		{"携带凭据", `{"url":"https://user:pass@example.com/a"}`},
		{"负 offset", `{"url":"https://example.com/a","offset":-1}`},
		{"limit 过大", `{"url":"https://example.com/a","limit":16001}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			all, up, _ := newTools(t)
			_, err := findTool(t, all, "web_ext_fetch").Call(context.Background(), json.RawMessage(tc.args))
			if err == nil {
				t.Fatal("应返回参数错误")
			}
			if len(up.calls()) != 0 {
				t.Fatal("参数校验失败时不应请求上游")
			}
		})
	}
}

func TestSearchAppliesDefaultLang(t *testing.T) {
	all, up, cache := newToolsWithLang(t, "zh")
	up.respond(200, `{"results":[{"title":"go","url":"https://go.dev","snippet":"short","summary":"sum","content":"body"}]}`)

	if _, err := findTool(t, all, "web_ext_search").Call(
		context.Background(), json.RawMessage(`{"query":"golang"}`)); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	calls := up.calls()
	if len(calls) != 1 {
		t.Fatalf("上游调用次数不符: %d", len(calls))
	}
	payload := decodeBody(t, calls[0].body)
	if payload["query"] != "golang lang:zh" {
		t.Fatalf("默认语言未补全: %#v", payload)
	}
	page, ok, err := cache.GetSummary(context.Background(), "https://go.dev")
	if err != nil || !ok {
		t.Fatalf("摘要未写入缓存: ok=%v err=%v", ok, err)
	}
	if page.SummaryQuery != "golang lang:zh" {
		t.Fatalf("缓存 summary_query 应使用补全后的 query，实际 %q", page.SummaryQuery)
	}
}

func TestSearchKeepsExplicitLang(t *testing.T) {
	all, up, _ := newToolsWithLang(t, "zh")
	up.respond(200, `{"results":[]}`)

	if _, err := findTool(t, all, "web_ext_search").Call(
		context.Background(), json.RawMessage(`{"query":"golang lang:en"}`)); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	payload := decodeBody(t, up.calls()[0].body)
	if payload["query"] != "golang lang:en" {
		t.Fatalf("显式 lang 不应被改动: %#v", payload)
	}
}

func TestSearchDirectAppliesDefaultLang(t *testing.T) {
	up := newFakeUpstream(t)
	up.respond(200, `{"results":[]}`)
	cache := newTestCache(t, time.Minute, 1<<20)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := SearchDirect(context.Background(),
		NewClient(up.server.URL, "test-key", DefaultOptions()), cache, logger,
		SearchOptions{Query: "golang", DefaultLang: "zh"}); err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	payload := decodeBody(t, up.calls()[0].body)
	if payload["query"] != "golang lang:zh" {
		t.Fatalf("CLI 默认语言未补全: %#v", payload)
	}
}

func TestSearchDirectSkipsTooLongDefaultLang(t *testing.T) {
	up := newFakeUpstream(t)
	up.respond(200, `{"results":[]}`)
	cache := newTestCache(t, time.Minute, 1<<20)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	long := strings.Repeat("a", maxQueryRunes)

	if _, err := SearchDirect(context.Background(),
		NewClient(up.server.URL, "test-key", DefaultOptions()), cache, logger,
		SearchOptions{Query: long, DefaultLang: "zh"}); err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	payload := decodeBody(t, up.calls()[0].body)
	if payload["query"] != long {
		t.Fatalf("超长时不应补全: %#v", payload)
	}
}
