package openapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultSearchCount = 10
	minSearchCount     = 5
	maxSearchCount     = 50

	defaultFetchLimit = 8000
	maxFetchLimit     = 16000

	// maxQueryRunes 是 query 的长度上限，与工具 schema 的 maxLength 一致。
	maxQueryRunes = 128

	// maxUsableSummaryRunes 是 summary 模式可接受的摘要长度上限。
	// 超过上限时不截断，直接视为未命中并回退为正文，保证返回的摘要始终完整。
	maxUsableSummaryRunes = 8000
)

// Tool 是 OpenAPI 工具层对外暴露的一项能力。
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Call        func(ctx context.Context, args json.RawMessage) ([]byte, error)
}

// Tools 返回 api/openapi.yaml 声明的全部工具。
// cache 为 nil 时工具仍可工作，只是不预热和读取缓存。
// defaultLang 非空时，query 未显式包含 lang: 的搜索会补全默认语言。
func Tools(client *Client, cache *Cache, logger *slog.Logger, defaultLang string) []Tool {
	if logger == nil {
		logger = slog.Default()
	}
	return []Tool{
		searchTool(client, cache, logger, defaultLang),
		fetchTool(client, cache, logger),
	}
}

func searchTool(client *Client, cache *Cache, logger *slog.Logger, defaultLang string) Tool {
	return Tool{
		Name: "web_ext_search",
		Description: `扩展网页搜索。可以按站点包含/排除、语言、发布时间范围过滤，也支持精确短语和更明确的检索目标。
返回 results 数组。每条结果包含 title、url、snippet、site_name、date，以及两个质量信号：
- rerank_score：0-3 的综合排序分，综合相关性、权威性和时效性；>=1.5 为优质结果。
- authority：0-4 的网页权威性，-1 表示未知；>=3 为极权威结果。
- date：发布时间（UTC，RFC 3339）；未知为 null。
只返回 snippet（通常 100-300 字符），不返回摘要和正文。
搜索时会把每条结果的正文与摘要写入本地缓存；需要摘要时对候选 URL 调用 web_ext_fetch(summary=true)，命中缓存时不会产生新的上游请求。`,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":      "string",
					"minLength": 1,
					"maxLength": 128,
					"description": `搜索词，1-128 个字符，不能全为空白。支持与关键词组合的搜索语法：
- site:example.com：限定站点；多个站点写作 site:a OR site:b。
- -site:example.com：排除指定站点。
- lang:zh,en：限定语言，多个用逗号分隔。
- after:2025 或 after:2025-01-01：限定发布时间之后。
- before:2026-06-01：限定发布时间之前。
- "精确短语"：精确匹配；-"排除短语"：排除该短语。
- 关键词前加减号：排除包含该关键词的网页。
示例：DeepSeek V4 site:github.com OR site:huggingface.co lang:zh after:2025
服务端可通过 default_lang 配置在未写 lang: 时自动补全（默认关闭）；已显式包含 lang: 的 query 不会被改动。`,
				},
				"count": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"maximum":     maxSearchCount,
					"default":     defaultSearchCount,
					"description": "返回结果条数，默认 10，最少 5，最多 50；0 或省略为 10，1-4 按 5 处理，负数报错。简单事实 5；一般 10；长尾冷门 25。subagent 或独立子任务可适当增大。",
				},
				"ori_question": map[string]any{
					"type":        []any{"string", "null"},
					"minLength":   1,
					"maxLength":   128,
					"description": "触发本次搜索的用户原始问题，1-128 个字符；用于帮助理解搜索意图。传 null 视为未提供。",
				},
				"localization": map[string]any{
					"type":        "string",
					"default":     "",
					"description": "用于优先返回本地化搜索结果的 IP 地址，例如 1.2.3.4；不提供时使用服务端默认。",
				},
			},
			"required": []string{"query"},
		},
		Call: func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			args, err := parseSearchArgs(raw)
			if err != nil {
				return nil, err
			}
			query, decision := applyDefaultLang(args.Query, defaultLang)
			logLangDecision(logger, decision, defaultLang, args.Query)
			body, err := client.Search(ctx, SearchRequest{
				Query:        query,
				Count:        args.Count,
				OriQuestion:  args.OriQuestion,
				Localization: args.Localization,
				NeedContent:  cache != nil,
			})
			if err != nil {
				return nil, err
			}
			return projectSearchResults(ctx, body, query, cache, logger)
		},
	}
}

// logLangDecision 记录默认语言补全的结果；不打印完整 query，避免日志泄露检索内容。
func logLangDecision(logger *slog.Logger, decision LangDecision, lang, query string) {
	switch decision {
	case langAppended:
		logger.Info("query 未指定 lang，已按默认语言补全", "lang", lang)
	case langTooLong:
		logger.Info("补全默认语言后超过长度上限，已跳过补全",
			"lang", lang, "query_runes", utf8.RuneCountInString(query))
	}
}

func fetchTool(client *Client, cache *Cache, logger *slog.Logger) Tool {
	return Tool{
		Name: "web_ext_fetch",
		Description: `扩展网页抓取。默认先查本地缓存，命中时直接返回；未命中或 refresh=true 时才调用上游。
summary=false：返回正文，按 offset/limit 分页。
summary=true：返回此前 web_ext_search 缓存的完整摘要，不分页；缓存中没有摘要，或摘要超过 8000 个字符时，回退为正文，并通过 hasSummary=false 与 hint 标明。
响应统一包含 contentLength（正文总字符数）、offset（偏移）、length（本次返回字符数）、hasSummary；正文模式还包含 hasMore 与 nextOffset。`,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   8192,
					"format":      "uri",
					"description": "要抓取的网页 URL，必须是包含主机名的 HTTP/HTTPS 地址，不能携带用户名或密码；长度不超过 8192 字符。",
				},
				"summary": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "默认 false，返回正文。true 时优先返回此前搜索缓存的摘要，不分页并忽略 offset/limit；没有可用摘要时回退为正文。",
				},
				"offset": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"default":     0,
					"description": "正文分页起点，按字符（rune）计，默认 0；summary=true 时忽略。",
				},
				"limit": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"maximum":     maxFetchLimit,
					"default":     defaultFetchLimit,
					"description": "正文分页长度，按字符（rune）计，默认 8000，最大 16000；0 视为 8000；summary=true 时忽略。",
				},
				"refresh": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "默认 false，优先使用缓存。true 时强制跳过缓存并重新抓取上游，同时丢弃该 URL 的旧摘要。",
				},
			},
			"required": []string{"url"},
		},
		Call: func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			opts, err := parseFetchArgs(raw)
			if err != nil {
				return nil, err
			}
			return FetchPage(ctx, client, cache, logger, opts)
		},
	}
}

type searchArgs struct {
	Query        string  `json:"query"`
	Count        int     `json:"count"`
	OriQuestion  *string `json:"ori_question"`
	Localization string  `json:"localization"`
}

func parseSearchArgs(raw json.RawMessage) (searchArgs, error) {
	var args searchArgs
	if err := decode(raw, &args); err != nil {
		return args, err
	}
	if strings.TrimSpace(args.Query) == "" {
		return args, errors.New("参数 query 不能为空")
	}
	if utf8.RuneCountInString(args.Query) > maxQueryRunes {
		return args, errors.New("参数 query 不能超过 128 个字符")
	}
	if args.Count < 0 {
		return args, errors.New("参数 count 不能小于 0")
	}
	if args.Count == 0 {
		args.Count = defaultSearchCount
	}
	if args.Count < minSearchCount {
		args.Count = minSearchCount
	}
	if args.Count > maxSearchCount {
		return args, fmt.Errorf("参数 count 不能超过 %d", maxSearchCount)
	}
	if args.OriQuestion != nil {
		if strings.TrimSpace(*args.OriQuestion) == "" {
			return args, errors.New("参数 ori_question 不能为空白")
		}
		if utf8.RuneCountInString(*args.OriQuestion) > 128 {
			return args, errors.New("参数 ori_question 不能超过 128 个字符")
		}
	}
	return args, nil
}

// FetchOptions 是 web_ext_fetch 的输入，也供命令行直接调用。
type FetchOptions struct {
	URL     string `json:"url"`
	Summary bool   `json:"summary"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit"`
	Refresh bool   `json:"refresh"`
	// FullContent 仅用于命令行：跳过 MCP 的分页上限，返回完整正文。
	// 使用 json:"-" 防止 MCP 调用方通过参数关闭分页保护。
	FullContent bool `json:"-"`
}

func parseFetchArgs(raw json.RawMessage) (FetchOptions, error) {
	var opts FetchOptions
	err := decode(raw, &opts)
	return opts, err
}

func validateFetchOptions(opts FetchOptions) (FetchOptions, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return opts, errors.New("参数 url 不能为空")
	}
	if utf8.RuneCountInString(opts.URL) > 8192 {
		return opts, errors.New("参数 url 不能超过 8192 个字符")
	}
	u, err := url.Parse(opts.URL)
	if err != nil {
		return opts, fmt.Errorf("参数 url 不合法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return opts, errors.New("参数 url 必须是 HTTP/HTTPS 地址")
	}
	if u.Host == "" {
		return opts, errors.New("参数 url 必须包含主机名")
	}
	if u.User != nil {
		return opts, errors.New("参数 url 不能携带用户名或密码")
	}
	if opts.Offset < 0 {
		return opts, errors.New("参数 offset 不能小于 0")
	}
	if opts.Limit == 0 {
		opts.Limit = defaultFetchLimit
	}
	if opts.Limit < 0 || opts.Limit > maxFetchLimit {
		return opts, fmt.Errorf("参数 limit 必须在 1 到 %d 之间", maxFetchLimit)
	}
	return opts, nil
}

// FetchPage 执行一次抓取，供 MCP 工具与命令行共用。
func FetchPage(ctx context.Context, client *Client, cache *Cache, logger *slog.Logger, opts FetchOptions) ([]byte, error) {
	if logger == nil {
		logger = slog.Default()
	}
	opts, err := validateFetchOptions(opts)
	if err != nil {
		return nil, err
	}
	if opts.Summary {
		return fetchSummary(ctx, client, cache, logger, opts)
	}
	return fetchContent(ctx, client, cache, logger, opts)
}

type searchResponse struct {
	Results []searchResult `json:"results"`
}

type searchResult struct {
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	Content     string  `json:"content"`
	SiteName    *string `json:"site_name"`
	Date        *string `json:"date"`
	RerankScore float64 `json:"rerank_score"`
	Authority   int     `json:"authority"`
}

type searchOutput struct {
	Results []searchOutputResult `json:"results"`
}

type searchOutputResult struct {
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	SiteName    *string `json:"site_name"`
	Date        *string `json:"date"`
	RerankScore float64 `json:"rerank_score"`
	Authority   int     `json:"authority"`
}

// projectSearchResults 会先把上游返回的正文与摘要写入缓存，再只输出 snippet。
func projectSearchResults(ctx context.Context, body []byte, query string, cache *Cache, logger *slog.Logger) ([]byte, error) {
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析搜索结果失败: %w", err)
	}
	if resp.Results == nil {
		resp.Results = []searchResult{}
	}
	if cache != nil {
		cacheSearchResults(ctx, cache, logger, query, resp.Results)
	}

	out := searchOutput{Results: make([]searchOutputResult, 0, len(resp.Results))}
	for _, r := range resp.Results {
		out.Results = append(out.Results, searchOutputResult{
			Title:       r.Title,
			URL:         r.URL,
			Snippet:     r.Snippet,
			SiteName:    r.SiteName,
			Date:        r.Date,
			RerankScore: r.RerankScore,
			Authority:   r.Authority,
		})
	}
	return json.Marshal(out)
}

type fetchResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

func cacheSearchResults(ctx context.Context, cache *Cache, logger *slog.Logger, query string, results []searchResult) {
	if cache == nil || len(results) == 0 {
		return
	}
	pages := make([]CachedPage, 0, len(results))
	for _, r := range results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		pages = append(pages, CachedPage{
			URL:          r.URL,
			Title:        r.Title,
			Content:      r.Content,
			Summary:      r.Summary,
			SummaryQuery: query,
		})
	}
	if err := cache.PutSearch(ctx, query, pages); err != nil {
		logger.Warn("写入搜索缓存失败", "err", err)
	}
}

// SearchOptions 是命令行直接搜索的输入。
type SearchOptions struct {
	Query          string
	Count          int
	IncludeContent bool
	// DefaultLang 非空时，query 未显式包含 lang: 会补全该语言码。
	DefaultLang string
}

type directSearchOutput struct {
	Results []directSearchResult `json:"results"`
}

type directSearchResult struct {
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	Content     string  `json:"content,omitempty"`
	SiteName    *string `json:"site_name"`
	Date        *string `json:"date"`
	RerankScore float64 `json:"rerank_score"`
	Authority   int     `json:"authority"`
}

// SearchDirect 供命令行直接调用：返回 summary，可选返回 content，并预热缓存。
func SearchDirect(ctx context.Context, client *Client, cache *Cache, logger *slog.Logger, opts SearchOptions) ([]byte, error) {
	if strings.TrimSpace(opts.Query) == "" {
		return nil, errors.New("参数 query 不能为空")
	}
	if utf8.RuneCountInString(opts.Query) > maxQueryRunes {
		return nil, errors.New("参数 query 不能超过 128 个字符")
	}
	if opts.Count < 0 {
		return nil, errors.New("参数 count 不能小于 0")
	}
	if opts.Count == 0 {
		opts.Count = defaultSearchCount
	}
	if opts.Count < minSearchCount {
		opts.Count = minSearchCount
	}
	if opts.Count > maxSearchCount {
		return nil, fmt.Errorf("参数 count 不能超过 %d", maxSearchCount)
	}
	if logger == nil {
		logger = slog.Default()
	}

	query, decision := applyDefaultLang(opts.Query, opts.DefaultLang)
	logLangDecision(logger, decision, opts.DefaultLang, opts.Query)

	body, err := client.Search(ctx, SearchRequest{
		Query:       query,
		Count:       opts.Count,
		NeedContent: cache != nil || opts.IncludeContent,
	})
	if err != nil {
		return nil, err
	}
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析搜索结果失败: %w", err)
	}
	if resp.Results == nil {
		resp.Results = []searchResult{}
	}
	cacheSearchResults(ctx, cache, logger, query, resp.Results)

	out := directSearchOutput{Results: make([]directSearchResult, 0, len(resp.Results))}
	for _, r := range resp.Results {
		item := directSearchResult{
			Title:       r.Title,
			URL:         r.URL,
			Snippet:     r.Snippet,
			Summary:     r.Summary,
			SiteName:    r.SiteName,
			Date:        r.Date,
			RerankScore: r.RerankScore,
			Authority:   r.Authority,
		}
		if opts.IncludeContent {
			item.Content = r.Content
		}
		out.Results = append(out.Results, item)
	}
	return json.Marshal(out)
}

type summaryOutput struct {
	Kind          string `json:"kind"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	Summary       string `json:"summary"`
	ContentLength int    `json:"contentLength"`
	Offset        int    `json:"offset"`
	Length        int    `json:"length"`
	HasSummary    bool   `json:"hasSummary"`
	SummaryQuery  string `json:"summaryQuery,omitempty"`
	Source        string `json:"source"`
	CachedAt      string `json:"cachedAt,omitempty"`
}

type contentOutput struct {
	Kind          string `json:"kind"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	Content       string `json:"content"`
	ContentLength int    `json:"contentLength"`
	Offset        int    `json:"offset"`
	Length        int    `json:"length"`
	HasMore       bool   `json:"hasMore"`
	NextOffset    *int   `json:"nextOffset,omitempty"`
	HasSummary    bool   `json:"hasSummary"`
	Source        string `json:"source"`
	CachedAt      string `json:"cachedAt,omitempty"`
	Hint          string `json:"hint,omitempty"`
}

func fetchSummary(ctx context.Context, client *Client, cache *Cache, logger *slog.Logger, opts FetchOptions) ([]byte, error) {
	summaryTooLong := false
	if !opts.Refresh && cache != nil {
		page, ok, err := cache.GetSummary(ctx, opts.URL)
		if err != nil {
			logger.Warn("读取摘要缓存失败", "url", opts.URL, "err", err)
		} else if ok {
			if utf8.RuneCountInString(page.Summary) <= maxUsableSummaryRunes {
				// 摘要始终完整返回；超过上限的摘要按未命中处理，回退为正文。
				return marshalSummaryOutput(page, "cache")
			}
			summaryTooLong = true
		}
	}

	page, source, err := loadContent(ctx, client, cache, logger, opts)
	if err != nil {
		return nil, err
	}
	// GetSummary 瞬时失败或 refresh 后，loadContent 可能已经拿到可用摘要，以实际状态为准。
	if page.HasSummary && utf8.RuneCountInString(page.Summary) <= maxUsableSummaryRunes {
		return marshalSummaryOutput(page, source)
	}
	hint := "该 URL 没有可用的缓存摘要；已返回正文。如需摘要，请先调用 web_ext_search。"
	if summaryTooLong || (page.Summary != "" && utf8.RuneCountInString(page.Summary) > maxUsableSummaryRunes) {
		// 重新搜索同一 URL 大概率仍返回同样超长的摘要，不要引导调用方循环搜索。
		hint = "缓存摘要超过 8000 个字符，无法以摘要形式返回；完整内容可按 offset/limit 分页读取正文。"
	}
	if opts.FullContent {
		return buildFullContentOutput(page, source, false, hint)
	}
	return buildContentOutput(page, 0, defaultFetchLimit, source, false, hint)
}

// marshalSummaryOutput 构造摘要模式的统一输出。
func marshalSummaryOutput(page CachedPage, source string) ([]byte, error) {
	out := summaryOutput{
		Kind:          "summary",
		URL:           page.URL,
		Title:         page.Title,
		Summary:       page.Summary,
		ContentLength: page.ContentLength,
		Offset:        0,
		Length:        utf8.RuneCountInString(page.Summary),
		HasSummary:    true,
		SummaryQuery:  page.SummaryQuery,
		Source:        source,
		CachedAt:      formatTime(page.SummaryAt),
	}
	return json.Marshal(out)
}

func fetchContent(ctx context.Context, client *Client, cache *Cache, logger *slog.Logger, opts FetchOptions) ([]byte, error) {
	page, source, err := loadContent(ctx, client, cache, logger, opts)
	if err != nil {
		return nil, err
	}
	if opts.FullContent {
		return buildFullContentOutput(page, source, page.HasSummary, "")
	}
	return buildContentOutput(page, opts.Offset, opts.Limit, source, page.HasSummary, "")
}

// loadContent 先读缓存，未命中或 refresh=true 时抓取上游并回写缓存。
func loadContent(ctx context.Context, client *Client, cache *Cache, logger *slog.Logger, opts FetchOptions) (CachedPage, string, error) {
	if !opts.Refresh && cache != nil {
		page, ok, err := cache.GetContent(ctx, opts.URL)
		if err != nil {
			logger.Warn("读取正文缓存失败", "url", opts.URL, "err", err)
		} else if ok {
			// 过长的摘要不作为可用摘要，避免调用方以为拿到的是完整内容。
			page.HasSummary = page.HasSummary && utf8.RuneCountInString(page.Summary) <= maxUsableSummaryRunes
			return page, "cache", nil
		}
	}

	body, err := client.Fetch(ctx, opts.URL)
	if err != nil {
		return CachedPage{}, "", err
	}
	var resp fetchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return CachedPage{}, "", fmt.Errorf("解析抓取结果失败: %w", err)
	}
	page := CachedPage{
		// 输出与缓存键保持一致，避免同一 URL 因大小写或 fragment 出现两种写法。
		URL:           cacheKey(opts.URL),
		Title:         resp.Title,
		Content:       resp.Content,
		ContentLength: utf8.RuneCountInString(resp.Content),
		ContentAt:     time.Now(),
	}
	if cache != nil {
		if resp.Content != "" {
			if err := cache.PutFetch(ctx, page, opts.Refresh); err != nil {
				logger.Warn("写入正文缓存失败", "url", opts.URL, "err", err)
			}
		} else if opts.Refresh {
			// 上游返回空正文时不写缓存，但 refresh 仍要丢弃旧摘要，避免继续返回过期摘要。
			if err := cache.ClearSummary(ctx, opts.URL); err != nil {
				logger.Warn("清除摘要缓存失败", "url", opts.URL, "err", err)
			}
		}
		if !opts.Refresh {
			if summary, ok, err := cache.GetSummary(ctx, opts.URL); err == nil && ok &&
				utf8.RuneCountInString(summary.Summary) <= maxUsableSummaryRunes {
				page.HasSummary = true
				page.Summary = summary.Summary
				page.SummaryAt = summary.SummaryAt
			}
		}
	}
	return page, "upstream", nil
}

func buildContentOutput(page CachedPage, offset, limit int, source string, hasSummary bool, hint string) ([]byte, error) {
	content, length := sliceRunes(page.Content, offset, limit)
	total := page.ContentLength
	if total == 0 && page.Content != "" {
		total = utf8.RuneCountInString(page.Content)
	}
	hasMore := offset+length < total
	var nextOffset *int
	if hasMore {
		next := offset + length
		nextOffset = &next
	}
	out := contentOutput{
		Kind:          "content",
		URL:           page.URL,
		Title:         page.Title,
		Content:       content,
		ContentLength: total,
		Offset:        offset,
		Length:        length,
		HasMore:       hasMore,
		NextOffset:    nextOffset,
		HasSummary:    hasSummary,
		Source:        source,
		CachedAt:      formatTime(page.ContentAt),
		Hint:          hint,
	}
	return json.Marshal(out)
}

// buildFullContentOutput 供命令行使用：不切页，一次性返回完整正文。
func buildFullContentOutput(page CachedPage, source string, hasSummary bool, hint string) ([]byte, error) {
	total := page.ContentLength
	if total == 0 && page.Content != "" {
		total = utf8.RuneCountInString(page.Content)
	}
	out := contentOutput{
		Kind:          "content",
		URL:           page.URL,
		Title:         page.Title,
		Content:       page.Content,
		ContentLength: total,
		Offset:        0,
		Length:        total,
		HasMore:       false,
		HasSummary:    hasSummary,
		Source:        source,
		CachedAt:      formatTime(page.ContentAt),
		Hint:          hint,
	}
	return json.Marshal(out)
}

// sliceRunes 按字符切分，避免切断 UTF-8 编码。
func sliceRunes(s string, offset, limit int) (string, int) {
	if offset < 0 || limit <= 0 {
		return "", 0
	}
	runes := []rune(s)
	if offset >= len(runes) {
		return "", 0
	}
	end := offset + limit
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[offset:end]), end - offset
}

func formatTime(t time.Time) string {
	// Unix 0 表示缓存时间为空，不能输出成 1970-01-01。
	if t.IsZero() || t.Unix() <= 0 {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("参数格式错误: %w", err)
	}
	return nil
}
