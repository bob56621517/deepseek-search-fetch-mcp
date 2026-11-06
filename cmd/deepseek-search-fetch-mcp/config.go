package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHost    = "127.0.0.1"
	defaultPort    = 18081
	defaultAPIURL  = "https://api.deepseek.com"
	dataDirName    = ".deepseek_search_fetch"
	configFileName = "config.json"

	defaultRequestTimeout                = 30 * time.Second
	defaultFetchTimeout                  = 60 * time.Second
	defaultCacheTTL                      = 30 * time.Minute
	defaultCacheMaxBytes           int64 = 1 << 30
	defaultHTTPMaxIdleConns              = 5
	defaultHTTPMaxIdleConnsPerHost       = 5
	defaultHTTPIdleConnTimeout           = 90 * time.Second
	defaultHTTPTLSHandshakeTimeout       = 10 * time.Second
	defaultResetConnectionOnError        = true
)

type config struct {
	apiKey  string
	apiURL  string
	host    string
	port    int
	dataDir string

	requestTimeout          time.Duration
	fetchTimeout            time.Duration
	cacheTTL                time.Duration
	cacheMaxBytes           int64
	defaultLang             string
	httpMaxIdleConns        int
	httpMaxIdleConnsPerHost int
	httpIdleConnTimeout     time.Duration
	httpTLSHandshakeTimeout time.Duration
	resetConnectionOnError  bool
}

type fileConfig struct {
	DeepSeekAPIKey          *string `json:"deepseek_api_key"`
	DeepSeekAPIURL          *string `json:"deepseek_api_url"`
	ServerHost              *string `json:"server_host"`
	ServerPort              *int    `json:"server_port"`
	RequestTimeout          *string `json:"request_timeout"`
	FetchTimeout            *string `json:"fetch_timeout"`
	CacheTTL                *string `json:"cache_ttl"`
	CacheMaxBytes           *int64  `json:"cache_max_bytes"`
	DefaultLang             *string `json:"default_lang"`
	HTTPMaxIdleConns        *int    `json:"http_max_idle_conns"`
	HTTPMaxIdleConnsPerHost *int    `json:"http_max_idle_conns_per_host"`
	HTTPIdleConnTimeout     *string `json:"http_idle_conn_timeout"`
	HTTPTLSHandshakeTimeout *string `json:"http_tls_handshake_timeout"`
	ResetConnectionOnError  *bool   `json:"reset_connection_on_error"`
}

// loadConfig 按优先级读取配置：启动参数 > 环境变量 > 固定配置文件 > 默认值。
// 配置只在启动时读取一次；首次启动时自动创建配置目录与配置文件。
// 主要供测试复用 server 级参数解析；各子命令入口使用 loadConfigFor。
func loadConfig(args []string, stderr io.Writer) (config, error) {
	fs, values := newFlagSet("deepseek-search-fetch-mcp", stderr)
	fs.StringVar(&values.host, "host", "", "监听地址")
	fs.IntVar(&values.port, "port", 0, "监听端口")
	fs.Usage = func() { printUsage(stderr) }
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("未知参数: %s", strings.Join(fs.Args(), " "))
	}
	return loadConfigFor(fs, values, true)
}

// flagValues 保存各子命令 FlagSet 解析出的值。
type flagValues struct {
	apiKey  string
	apiURL  string
	host    string
	port    int
	dataDir string
}

func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *flagValues) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	values := &flagValues{}
	fs.StringVar(&values.apiKey, "deepseek-api-key", "", "DeepSeek API Key")
	fs.StringVar(&values.apiURL, "deepseek-api-url", "", "DeepSeek API Base URL")
	fs.StringVar(&values.dataDir, "data-dir", "", "数据目录（存放配置、日志与缓存）")
	return fs, values
}

// loadConfigFor 在 FlagSet 解析完成后合并配置文件与环境变量。
func loadConfigFor(fs *flag.FlagSet, values *flagValues, requireKey bool) (config, error) {
	cfg := config{
		apiURL:                  defaultAPIURL,
		host:                    defaultHost,
		port:                    defaultPort,
		requestTimeout:          defaultRequestTimeout,
		fetchTimeout:            defaultFetchTimeout,
		cacheTTL:                defaultCacheTTL,
		cacheMaxBytes:           defaultCacheMaxBytes,
		httpMaxIdleConns:        defaultHTTPMaxIdleConns,
		httpMaxIdleConnsPerHost: defaultHTTPMaxIdleConnsPerHost,
		httpIdleConnTimeout:     defaultHTTPIdleConnTimeout,
		httpTLSHandshakeTimeout: defaultHTTPTLSHandshakeTimeout,
		resetConnectionOnError:  defaultResetConnectionOnError,
	}
	setByFlag := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setByFlag[f.Name] = true })

	// 数据目录：启动参数 > 环境变量 > 默认 ~/.deepseek_search_fetch
	cfg.dataDir = resolveDataDir(dataDirOverride(setByFlag, values))

	path, err := ensureConfigFile(cfg.dataDir)
	if err != nil {
		return cfg, err
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
		}
		var file fileConfig
		if err := json.Unmarshal(data, &file); err != nil {
			return cfg, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
		}
		if err := applyFileConfig(&cfg, file); err != nil {
			return cfg, err
		}
	}

	if v, ok := os.LookupEnv("DEEPSEEK_API_KEY"); ok && strings.TrimSpace(v) != "" {
		cfg.apiKey = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("DEEPSEEK_API_URL"); ok && strings.TrimSpace(v) != "" {
		cfg.apiURL = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("DEEPSEEK_SEARCH_FETCH_HOST"); ok && strings.TrimSpace(v) != "" {
		cfg.host = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("DEEPSEEK_SEARCH_FETCH_PORT"); ok && strings.TrimSpace(v) != "" {
		p, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return cfg, fmt.Errorf("环境变量 DEEPSEEK_SEARCH_FETCH_PORT 不是合法端口: %q", v)
		}
		cfg.port = p
	}
	if v, ok := os.LookupEnv("DEEPSEEK_DEFAULT_LANG"); ok && strings.TrimSpace(v) != "" {
		cfg.defaultLang = strings.TrimSpace(v)
	}

	if setByFlag["deepseek-api-key"] {
		cfg.apiKey = strings.TrimSpace(values.apiKey)
	}
	if setByFlag["deepseek-api-url"] {
		cfg.apiURL = strings.TrimSpace(values.apiURL)
	}
	if setByFlag["host"] {
		cfg.host = values.host
	}
	if setByFlag["port"] {
		cfg.port = values.port
	}

	if err := validateConfig(&cfg); err != nil {
		return cfg, err
	}
	if requireKey && cfg.apiKey == "" {
		hint := "请设置 DEEPSEEK_API_KEY 或 --deepseek-api-key"
		if path != "" {
			hint += "，或写入 " + path + " 的 deepseek_api_key"
		}
		return cfg, errors.New("未配置 DeepSeek API Key：" + hint)
	}
	return cfg, nil
}

func applyFileConfig(cfg *config, file fileConfig) error {
	if file.DeepSeekAPIKey != nil {
		cfg.apiKey = strings.TrimSpace(*file.DeepSeekAPIKey)
	}
	if file.DeepSeekAPIURL != nil {
		cfg.apiURL = strings.TrimSpace(*file.DeepSeekAPIURL)
	}
	if file.ServerHost != nil {
		cfg.host = strings.TrimSpace(*file.ServerHost)
	}
	if file.ServerPort != nil {
		cfg.port = *file.ServerPort
	}
	if file.RequestTimeout != nil {
		d, err := parseDuration("request_timeout", *file.RequestTimeout)
		if err != nil {
			return err
		}
		cfg.requestTimeout = d
	}
	if file.FetchTimeout != nil {
		d, err := parseDuration("fetch_timeout", *file.FetchTimeout)
		if err != nil {
			return err
		}
		cfg.fetchTimeout = d
	}
	if file.CacheTTL != nil {
		d, err := parseDuration("cache_ttl", *file.CacheTTL)
		if err != nil {
			return err
		}
		cfg.cacheTTL = d
	}
	if file.CacheMaxBytes != nil {
		cfg.cacheMaxBytes = *file.CacheMaxBytes
	}
	if file.DefaultLang != nil {
		cfg.defaultLang = strings.TrimSpace(*file.DefaultLang)
	}
	if file.HTTPMaxIdleConns != nil {
		cfg.httpMaxIdleConns = *file.HTTPMaxIdleConns
	}
	if file.HTTPMaxIdleConnsPerHost != nil {
		cfg.httpMaxIdleConnsPerHost = *file.HTTPMaxIdleConnsPerHost
	}
	if file.HTTPIdleConnTimeout != nil {
		d, err := parseDuration("http_idle_conn_timeout", *file.HTTPIdleConnTimeout)
		if err != nil {
			return err
		}
		cfg.httpIdleConnTimeout = d
	}
	if file.HTTPTLSHandshakeTimeout != nil {
		d, err := parseDuration("http_tls_handshake_timeout", *file.HTTPTLSHandshakeTimeout)
		if err != nil {
			return err
		}
		cfg.httpTLSHandshakeTimeout = d
	}
	if file.ResetConnectionOnError != nil {
		cfg.resetConnectionOnError = *file.ResetConnectionOnError
	}
	return nil
}

func parseDuration(name, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("配置项 %s 不是合法时长: %q", name, raw)
	}
	return d, nil
}

// durationText 输出更直观的时长文本，例如 90s 而不是 1m30s。
func durationText(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

func validateConfig(cfg *config) error {
	if cfg.host == "" {
		cfg.host = defaultHost
	}
	if cfg.port < 1 || cfg.port > 65535 {
		return fmt.Errorf("端口必须在 1-65535 之间，当前为 %d", cfg.port)
	}
	if cfg.apiURL == "" {
		cfg.apiURL = defaultAPIURL
	}
	u, err := url.Parse(cfg.apiURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("DeepSeek API URL 不合法: %q", cfg.apiURL)
	}
	if cfg.requestTimeout <= 0 {
		return fmt.Errorf("request_timeout 必须大于 0，当前为 %s", cfg.requestTimeout)
	}
	if cfg.fetchTimeout <= 0 {
		return fmt.Errorf("fetch_timeout 必须大于 0，当前为 %s", cfg.fetchTimeout)
	}
	if cfg.cacheTTL <= 0 {
		return fmt.Errorf("cache_ttl 必须大于 0，当前为 %s", cfg.cacheTTL)
	}
	if cfg.cacheMaxBytes < 1<<20 {
		return fmt.Errorf("cache_max_bytes 不能小于 1048576，当前为 %d", cfg.cacheMaxBytes)
	}
	switch {
	case cfg.defaultLang == "":
	case strings.EqualFold(cfg.defaultLang, "auto"):
		cfg.defaultLang = "auto"
	default:
		code := strings.ToLower(cfg.defaultLang)
		if len(code) != 2 || code[0] < 'a' || code[0] > 'z' || code[1] < 'a' || code[1] > 'z' {
			return fmt.Errorf("default_lang 只能为空、auto 或两位语言码（如 zh），当前为 %q", cfg.defaultLang)
		}
		cfg.defaultLang = code
	}
	if cfg.httpMaxIdleConns < 1 {
		return fmt.Errorf("http_max_idle_conns 必须大于等于 1，当前为 %d", cfg.httpMaxIdleConns)
	}
	if cfg.httpMaxIdleConnsPerHost < 1 {
		return fmt.Errorf("http_max_idle_conns_per_host 必须大于等于 1，当前为 %d", cfg.httpMaxIdleConnsPerHost)
	}
	if cfg.httpIdleConnTimeout <= 0 {
		return fmt.Errorf("http_idle_conn_timeout 必须大于 0，当前为 %s", cfg.httpIdleConnTimeout)
	}
	if cfg.httpTLSHandshakeTimeout <= 0 {
		return fmt.Errorf("http_tls_handshake_timeout 必须大于 0，当前为 %s", cfg.httpTLSHandshakeTimeout)
	}
	return nil
}

// ensureConfigFile 创建配置目录；配置文件不存在时写入模板。
// 无法确定用户主目录时跳过文件，仅使用环境变量或启动参数。
func ensureConfigFile(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建配置目录 %s 失败: %w", dir, err)
	}

	path := filepath.Join(dir, configFileName)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("检查配置文件 %s 失败: %w", path, err)
	}

	content, err := json.MarshalIndent(struct {
		DeepSeekAPIKey          string `json:"deepseek_api_key"`
		DeepSeekAPIURL          string `json:"deepseek_api_url"`
		ServerHost              string `json:"server_host"`
		ServerPort              int    `json:"server_port"`
		RequestTimeout          string `json:"request_timeout"`
		FetchTimeout            string `json:"fetch_timeout"`
		CacheTTL                string `json:"cache_ttl"`
		CacheMaxBytes           int64  `json:"cache_max_bytes"`
		DefaultLang             string `json:"default_lang"`
		HTTPMaxIdleConns        int    `json:"http_max_idle_conns"`
		HTTPMaxIdleConnsPerHost int    `json:"http_max_idle_conns_per_host"`
		HTTPIdleConnTimeout     string `json:"http_idle_conn_timeout"`
		HTTPTLSHandshakeTimeout string `json:"http_tls_handshake_timeout"`
		ResetConnectionOnError  bool   `json:"reset_connection_on_error"`
	}{
		DeepSeekAPIKey:          "",
		DeepSeekAPIURL:          defaultAPIURL,
		ServerHost:              defaultHost,
		ServerPort:              defaultPort,
		RequestTimeout:          durationText(defaultRequestTimeout),
		FetchTimeout:            durationText(defaultFetchTimeout),
		CacheTTL:                durationText(defaultCacheTTL),
		CacheMaxBytes:           defaultCacheMaxBytes,
		DefaultLang:             "",
		HTTPMaxIdleConns:        defaultHTTPMaxIdleConns,
		HTTPMaxIdleConnsPerHost: defaultHTTPMaxIdleConnsPerHost,
		HTTPIdleConnTimeout:     durationText(defaultHTTPIdleConnTimeout),
		HTTPTLSHandshakeTimeout: durationText(defaultHTTPTLSHandshakeTimeout),
		ResetConnectionOnError:  defaultResetConnectionOnError,
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("生成配置文件模板失败: %w", err)
	}
	if err := os.WriteFile(path, append(content, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("创建配置文件 %s 失败: %w", path, err)
	}
	return path, nil
}

// dataDirOverride 返回显式指定的数据目录：启动参数优先于环境变量，均未指定时返回空。
func dataDirOverride(setByFlag map[string]bool, values *flagValues) string {
	if setByFlag["data-dir"] {
		return strings.TrimSpace(values.dataDir)
	}
	if v, ok := os.LookupEnv("DEEPSEEK_SEARCH_FETCH_HOME"); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// resolveDataDir 确定数据目录：显式指定优先，否则回退到用户主目录下的默认目录。
// 无法确定主目录且无显式指定时返回空字符串。
func resolveDataDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, dataDirName)
}
