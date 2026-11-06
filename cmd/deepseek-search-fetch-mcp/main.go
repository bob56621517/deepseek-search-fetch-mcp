// deepseek-search-fetch-mcp 把 DeepSeek 的 Search 与 Fetch 接口暴露为 MCP 工具，
// 同时提供直接调用与缓存管理命令。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bob56621517/deepseek-search-fetch-mcp/internal/auth"
	"github.com/bob56621517/deepseek-search-fetch-mcp/internal/mcp"
	"github.com/bob56621517/deepseek-search-fetch-mcp/internal/openapi"
)

var (
	version      = "dev"
	buildChannel = "local"
)

func buildVersion() string {
	if buildChannel == "" {
		return version
	}
	return version + "+" + buildChannel
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage(os.Stderr)
		return errors.New("必须指定命令：server、search、fetch、flushall 或 apikey")
	}

	var err error
	switch args[0] {
	case "server":
		err = runServer(args[1:])
	case "search":
		err = runSearch(args[1:])
	case "fetch":
		err = runFetch(args[1:])
	case "flushall":
		err = runFlushAll(args[1:])
	case "apikey":
		err = runAPIKey(args[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return nil
	default:
		printUsage(os.Stderr)
		return fmt.Errorf("未知命令: %s", args[0])
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `用法:
  deepseek-search-fetch-mcp server   [--host 127.0.0.1] [--port 18081]                 启动 MCP 服务
  deepseek-search-fetch-mcp search   [--count 10] [--content] <query>                  直接搜索并输出 JSON
  deepseek-search-fetch-mcp fetch    [--summary] [--refresh] <url>                    直接抓取完整正文并输出 JSON
  deepseek-search-fetch-mcp flushall                                                   清空本地缓存
  deepseek-search-fetch-mcp apikey   add <name> | list | remove <name>                管理 MCP 访问密钥

全局参数（写在命令之后）:
  --deepseek-api-key string   DeepSeek API Key
  --deepseek-api-url string   DeepSeek API Base URL
  --data-dir string           数据目录（默认 ~/.deepseek_search_fetch）

每个命令都支持 --help 查看细节。
配置优先级：命令行 > 环境变量 > ~/.deepseek_search_fetch/config.json > 默认值。
命令行模式下日志只写入日志文件，stdout 仅输出 JSON。
`)
}

func runServer(args []string) error {
	fs, values := newFlagSet("server", os.Stderr)
	fs.StringVar(&values.host, "host", "", "监听地址")
	fs.IntVar(&values.port, "port", 0, "监听端口")
	setCommandUsage(fs, "server [--host 127.0.0.1] [--port 18081]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("server 不接受额外参数: %s", strings.Join(fs.Args(), " "))
	}
	cfg, err := loadConfigFor(fs, values, true)
	if err != nil {
		return err
	}
	return serve(cfg)
}

// resolveDefaultLang 把配置中的 auto 解析为运行环境的主语言码。
// 解析为空时自动语言补全关闭，并记录一条日志便于排查。
func resolveDefaultLang(value string, logger *slog.Logger) string {
	if value != "auto" {
		return value
	}
	code := openapi.DetectLocale()
	if code == "" {
		logger.Info("未检测到语言环境变量，自动语言补全关闭")
		return ""
	}
	logger.Info("已启用默认语言自动补全", "lang", code)
	return code
}

func serve(cfg config) error {
	logger, logCloser, err := newLogger(os.Stderr, slog.LevelInfo, cfg.dataDir)
	if err != nil {
		return err
	}
	defer logCloser.Close()
	slog.SetDefault(logger)

	authStore, err := openAuthStore(cfg, logger)
	if err != nil {
		return fmt.Errorf("打开访问密钥库失败: %w", err)
	}
	defer authStore.Close()

	hasKeys, err := authStore.HasKeys(context.Background())
	if err != nil {
		return fmt.Errorf("读取访问密钥失败: %w", err)
	}
	if !hasKeys && !isLoopbackHost(cfg.host) {
		logger.Warn("未配置访问密钥，/mcp 不校验身份；如非本机使用请先执行 apikey add", "host", cfg.host)
	}

	resolver, err := auth.NewClientIPResolver(cfg.trustedProxies)
	if err != nil {
		return err
	}
	authMiddleware, err := auth.NewMiddleware(auth.MiddlewareOptions{
		Store:    authStore,
		Resolver: resolver,
		Limiter: auth.NewLimiter(auth.LimitConfig{
			KeyRPS:         cfg.rateLimitRPS,
			KeyBurst:       cfg.rateLimitBurst,
			AuthFailLimit:  cfg.authFailLimit,
			AuthFailWindow: cfg.authFailWindow,
			AuthFailBlock:  cfg.authFailBlock,
			IPRPS:          cfg.ipRateLimitRPS,
			IPBurst:        cfg.ipRateLimitBurst,
		}),
		Logger: logger,
	})
	if err != nil {
		return err
	}
	if err := authMiddleware.RefreshMode(context.Background()); err != nil {
		return fmt.Errorf("读取访问密钥失败: %w", err)
	}

	client := newAPIClient(cfg)
	defer client.CloseIdleConnections()

	cache, err := openCache(cfg, logger)
	if err != nil {
		logger.Warn("缓存不可用，将在无缓存模式下运行", "err", err)
		cache = nil
	} else {
		defer cache.Close()
	}

	defaultLang := resolveDefaultLang(cfg.defaultLang, logger)
	srv := mcp.New(mcp.Options{
		Client:      client,
		Cache:       cache,
		Logger:      logger,
		Version:     buildVersion(),
		DefaultLang: defaultLang,
		Auth:        authMiddleware,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port))
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("关闭 HTTP 服务失败", "err", err)
		}
	}()

	cfgPath := ""
	cachePath := ""
	authPath := ""
	if cfg.dataDir != "" {
		cfgPath = filepath.Join(cfg.dataDir, configFileName)
		cachePath = filepath.Join(cfg.dataDir, "cache.db")
		authPath = filepath.Join(cfg.dataDir, authDBFileName)
	}
	logger.Info("服务启动", "addr", addr, "version", version, "build", buildChannel,
		"config", cfgPath, "data_dir", cfg.dataDir, "cache", cachePath, "auth_db", authPath)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runSearch(args []string) error {
	fs, values := newFlagSet("search", os.Stderr)
	count := fs.Int("count", 10, "返回结果条数，默认 10，最少 5，最多 50")
	content := fs.Bool("content", false, "同时返回网页正文")
	setCommandUsage(fs, "search [--count 10] [--content] <query>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("search 需要 query 参数")
	}
	cfg, err := loadConfigFor(fs, values, true)
	if err != nil {
		return err
	}

	logger, logCloser := newCLILogger(cfg.dataDir)
	defer logCloser.Close()
	client := newAPIClient(cfg)
	defer client.CloseIdleConnections()
	cache, err := openCache(cfg, logger)
	if err != nil {
		logger.Warn("缓存不可用，命令将在无缓存模式下运行", "err", err)
	} else {
		defer cache.Close()
	}

	defaultLang := resolveDefaultLang(cfg.defaultLang, logger)
	body, err := openapi.SearchDirect(context.Background(), client, cache, logger, openapi.SearchOptions{
		Query:          query,
		Count:          *count,
		IncludeContent: *content,
		DefaultLang:    defaultLang,
	})
	if err != nil {
		return err
	}
	return printJSON(body)
}

func runFetch(args []string) error {
	fs, values := newFlagSet("fetch", os.Stderr)
	summary := fs.Bool("summary", false, "优先返回缓存摘要，不分页")
	refresh := fs.Bool("refresh", false, "强制跳过缓存并刷新")
	setCommandUsage(fs, "fetch [--summary] [--refresh] <url>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("fetch 需要且只接受一个 url 参数")
	}
	cfg, err := loadConfigFor(fs, values, true)
	if err != nil {
		return err
	}

	logger, logCloser := newCLILogger(cfg.dataDir)
	defer logCloser.Close()
	client := newAPIClient(cfg)
	defer client.CloseIdleConnections()
	cache, err := openCache(cfg, logger)
	if err != nil {
		logger.Warn("缓存不可用，命令将在无缓存模式下运行", "err", err)
	} else {
		defer cache.Close()
	}

	body, err := openapi.FetchPage(context.Background(), client, cache, logger, openapi.FetchOptions{
		URL:         fs.Arg(0),
		Summary:     *summary,
		Refresh:     *refresh,
		FullContent: true,
	})
	if err != nil {
		return err
	}
	return printJSON(body)
}

func runFlushAll(args []string) error {
	fs, values := newFlagSet("flushall", os.Stderr)
	setCommandUsage(fs, "flushall")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("flushall 不接受额外参数: %s", strings.Join(fs.Args(), " "))
	}
	cfg, err := loadConfigFor(fs, values, false)
	if err != nil {
		return err
	}
	logger, logCloser := newCLILogger(cfg.dataDir)
	defer logCloser.Close()
	cache, err := openCache(cfg, logger)
	if err != nil {
		return err
	}
	defer cache.Close()
	if err := cache.Flush(context.Background()); err != nil {
		return err
	}
	return printJSON([]byte(`{"flushed":true}`))
}

func runAPIKey(args []string) error {
	if len(args) == 0 {
		printAPIKeyUsage(os.Stderr)
		return errors.New("apikey 需要子命令：add、list 或 remove")
	}
	switch args[0] {
	case "add":
		return runAPIKeyAdd(args[1:])
	case "list":
		return runAPIKeyList(args[1:])
	case "remove":
		return runAPIKeyRemove(args[1:])
	case "help", "-h", "--help":
		printAPIKeyUsage(os.Stdout)
		return nil
	default:
		printAPIKeyUsage(os.Stderr)
		return fmt.Errorf("未知 apikey 子命令: %s", args[0])
	}
}

func printAPIKeyUsage(w io.Writer) {
	fmt.Fprint(w, `用法:
  deepseek-search-fetch-mcp apikey add <name>      生成访问密钥（只显示一次）
  deepseek-search-fetch-mcp apikey list            列出访问密钥（首尾脱敏）
  deepseek-search-fetch-mcp apikey remove <name>   删除访问密钥

说明:
  - 访问密钥用于 MCP 客户端访问 /mcp，与上游 DeepSeek API Key 无关。
  - 没有访问密钥时 /mcp 不校验身份；至少有一个密钥后强制校验。
  - 密钥只保存哈希，完整值只在 add 时显示一次。
  - 全局参数（如 --data-dir）写在 apikey 子命令之后、name 之前。
`)
}

type apiKeyAddOutput struct {
	Name      string `json:"name"`
	APIKey    string `json:"api_key"`
	Masked    string `json:"masked"`
	CreatedAt string `json:"created_at"`
	Auth      string `json:"auth"`
	Warning   string `json:"warning"`
}

func runAPIKeyAdd(args []string) error {
	fs, values := newFlagSet("apikey add", os.Stderr)
	setCommandUsage(fs, "apikey add <name>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("apikey add 需要且只接受一个 name 参数")
	}
	name := fs.Arg(0)
	cfg, err := loadConfigFor(fs, values, false)
	if err != nil {
		return err
	}

	logger, logCloser := newCLILogger(cfg.dataDir)
	defer logCloser.Close()
	store, err := openAuthStore(cfg, logger)
	if err != nil {
		return err
	}
	defer store.Close()

	key, plain, err := store.Add(context.Background(), name)
	if err != nil {
		return err
	}
	logger.Info("添加访问密钥", "name", key.Name, "masked", key.Masked())
	body, err := json.Marshal(apiKeyAddOutput{
		Name:      key.Name,
		APIKey:    plain,
		Masked:    key.Masked(),
		CreatedAt: formatAuthTime(key.CreatedAt),
		Auth:      "enforced",
		Warning:   "访问密钥只显示一次，请立即保存",
	})
	if err != nil {
		return err
	}
	return printJSON(body)
}

type apiKeyView struct {
	Name       string `json:"name"`
	Masked     string `json:"masked"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
}

type apiKeyListOutput struct {
	Auth string       `json:"auth"`
	Keys []apiKeyView `json:"keys"`
}

func runAPIKeyList(args []string) error {
	fs, values := newFlagSet("apikey list", os.Stderr)
	setCommandUsage(fs, "apikey list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("apikey list 不接受额外参数: %s", strings.Join(fs.Args(), " "))
	}
	cfg, err := loadConfigFor(fs, values, false)
	if err != nil {
		return err
	}

	logger, logCloser := newCLILogger(cfg.dataDir)
	defer logCloser.Close()
	store, err := openAuthStore(cfg, logger)
	if err != nil {
		return err
	}
	defer store.Close()

	keys, err := store.List(context.Background())
	if err != nil {
		return err
	}
	views := make([]apiKeyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, apiKeyView{
			Name:       key.Name,
			Masked:     key.Masked(),
			CreatedAt:  formatAuthTime(key.CreatedAt),
			LastUsedAt: formatAuthTime(key.LastUsedAt),
		})
	}
	mode := "disabled"
	if len(keys) > 0 {
		mode = "enforced"
	}
	body, err := json.Marshal(apiKeyListOutput{Auth: mode, Keys: views})
	if err != nil {
		return err
	}
	return printJSON(body)
}

type apiKeyRemoveOutput struct {
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
	Auth    string `json:"auth"`
	Warning string `json:"warning,omitempty"`
}

func runAPIKeyRemove(args []string) error {
	fs, values := newFlagSet("apikey remove", os.Stderr)
	setCommandUsage(fs, "apikey remove <name>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("apikey remove 需要且只接受一个 name 参数")
	}
	name := fs.Arg(0)
	cfg, err := loadConfigFor(fs, values, false)
	if err != nil {
		return err
	}

	logger, logCloser := newCLILogger(cfg.dataDir)
	defer logCloser.Close()
	store, err := openAuthStore(cfg, logger)
	if err != nil {
		return err
	}
	defer store.Close()

	key, removed, err := store.Remove(context.Background(), name)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("访问密钥不存在: %s", strings.TrimSpace(name))
	}
	hasKeys, err := store.HasKeys(context.Background())
	if err != nil {
		return err
	}
	mode := "disabled"
	warning := ""
	if hasKeys {
		mode = "enforced"
	} else {
		warning = "已删除最后一个访问密钥，/mcp 将不再校验身份"
	}
	logger.Info("删除访问密钥", "name", key.Name, "masked", key.Masked(), "auth", mode)
	body, err := json.Marshal(apiKeyRemoveOutput{
		Name:    key.Name,
		Removed: true,
		Auth:    mode,
		Warning: warning,
	})
	if err != nil {
		return err
	}
	return printJSON(body)
}

func formatAuthTime(t time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// isLoopbackHost 判断监听地址是否只对本机可见；无法解析的主机名按非本机处理。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func openAuthStore(cfg config, logger *slog.Logger) (*auth.Store, error) {
	if cfg.dataDir == "" {
		return nil, errors.New("无法确定数据目录，访问密钥库不可用；请使用 --data-dir 或 DEEPSEEK_SEARCH_FETCH_HOME 指定")
	}
	return auth.OpenStore(auth.StoreOptions{
		Path:   filepath.Join(cfg.dataDir, authDBFileName),
		Logger: logger,
	})
}

func newAPIClient(cfg config) *openapi.Client {
	return openapi.NewClient(cfg.apiURL, cfg.apiKey, openapi.Options{
		Timeout:             cfg.requestTimeout,
		FetchTimeout:        cfg.fetchTimeout,
		MaxIdleConns:        cfg.httpMaxIdleConns,
		MaxIdleConnsPerHost: cfg.httpMaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.httpIdleConnTimeout,
		TLSHandshakeTimeout: cfg.httpTLSHandshakeTimeout,
		ResetOnError:        cfg.resetConnectionOnError,
	})
}

func openCache(cfg config, logger *slog.Logger) (*openapi.Cache, error) {
	dir := cfg.dataDir
	if dir == "" {
		return nil, errors.New("无法确定数据目录，缓存不可用")
	}
	return openapi.OpenCache(openapi.CacheOptions{
		Path:     filepath.Join(dir, "cache.db"),
		TTL:      cfg.cacheTTL,
		MaxBytes: cfg.cacheMaxBytes,
		Logger:   logger,
	})
}

// newCLILogger 返回只写日志文件的 logger，保证 stdout 只输出 JSON。
func newCLILogger(dataDir string) (*slog.Logger, io.Closer) {
	logger, closer, err := newLogger(nil, slog.LevelInfo, dataDir)
	if err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), nopCloser{}
	}
	return logger, closer
}

func setCommandUsage(fs *flag.FlagSet, usage string) {
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "用法: deepseek-search-fetch-mcp %s\n", usage)
		fs.PrintDefaults()
	}
}

func printJSON(body []byte) error {
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err != nil {
		_, err = os.Stdout.Write(append(body, '\n'))
		return err
	}
	out.WriteByte('\n')
	_, err := os.Stdout.Write(out.Bytes())
	return err
}
