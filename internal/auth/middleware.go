package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxCredentialLen = 512

// MiddlewareOptions 是鉴权中间件的装配参数。
type MiddlewareOptions struct {
	Store    *Store
	Limiter  *Limiter
	Resolver *ClientIPResolver
	Logger   *slog.Logger
}

// Middleware 包装 /mcp 处理链，提供访问密钥鉴权、限流与审计日志。
type Middleware struct {
	store    *Store
	limiter  *Limiter
	resolver *ClientIPResolver
	logger   *slog.Logger

	modeMu    sync.Mutex
	modeKnown bool
	enforced  bool
}

// NewMiddleware 构建鉴权中间件。Store 必填，其余参数为空时使用安全默认值。
func NewMiddleware(opts MiddlewareOptions) (*Middleware, error) {
	if opts.Store == nil {
		return nil, errors.New("鉴权存储不能为空")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	resolver := opts.Resolver
	if resolver == nil {
		var err error
		resolver, err = NewClientIPResolver(nil)
		if err != nil {
			return nil, err
		}
	}
	limiter := opts.Limiter
	if limiter == nil {
		limiter = NewLimiter(LimitConfig{})
	}
	return &Middleware{
		store:    opts.Store,
		limiter:  limiter,
		resolver: resolver,
		logger:   logger,
	}, nil
}

// RefreshMode 读取当前密钥数量并记录鉴权模式，适合在服务启动时调用。
func (m *Middleware) RefreshMode(ctx context.Context) error {
	hasKeys, err := m.store.HasKeys(ctx)
	if err != nil {
		return err
	}
	m.setMode(hasKeys)
	return nil
}

// Wrap 返回带鉴权与限流的处理器。
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, peer := m.resolver.Resolve(r)

		if allowed, retryAfter := m.limiter.AllowIP(client); !allowed {
			m.reject(w, r, client, peer, http.StatusTooManyRequests,
				"ip_rate_limited", "ip_rate_limited", retryAfter, "", nil)
			return
		}

		hasKeys, err := m.store.HasKeys(r.Context())
		if err != nil {
			m.reject(w, r, client, peer, http.StatusServiceUnavailable,
				"auth_unavailable", "store_error", 0, "", err)
			return
		}
		m.setMode(hasKeys)
		if !hasKeys {
			// 没有访问密钥时静默提供服务，不做鉴权，也不套用鉴权失败封禁。
			next.ServeHTTP(w, r)
			return
		}

		if blocked, retryAfter := m.limiter.AuthBlocked(client); blocked {
			m.reject(w, r, client, peer, http.StatusTooManyRequests,
				"auth_blocked", "too_many_auth_failures", retryAfter, "", nil)
			return
		}

		plain, reason := credentialFromRequest(r)
		if reason != "" {
			m.limiter.RecordAuthFailure(client)
			m.reject(w, r, client, peer, http.StatusUnauthorized,
				"auth_fail", reason, 0, "", nil)
			return
		}

		key, ok, err := m.store.Authenticate(r.Context(), plain)
		if err != nil {
			m.reject(w, r, client, peer, http.StatusServiceUnavailable,
				"auth_unavailable", "store_error", 0, "", err)
			return
		}
		if !ok {
			m.limiter.RecordAuthFailure(client)
			m.reject(w, r, client, peer, http.StatusUnauthorized,
				"auth_fail", "invalid_key", 0, "", nil)
			return
		}

		m.limiter.RecordAuthSuccess(client)
		if allowed, retryAfter := m.limiter.AllowKey(key.ID); !allowed {
			m.reject(w, r, client, peer, http.StatusTooManyRequests,
				"key_rate_limited", "key_rate_limited", retryAfter, key.Name, nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// setMode 只在启动时和模式变化时记录日志，避免每个请求刷屏。
func (m *Middleware) setMode(enforced bool) {
	m.modeMu.Lock()
	defer m.modeMu.Unlock()

	if !m.modeKnown {
		m.modeKnown = true
		m.enforced = enforced
		if enforced {
			m.logger.Info("访问鉴权已启用，/mcp 请求必须携带访问密钥")
		} else {
			m.logger.Info("访问鉴权未启用，/mcp 不校验访问密钥")
		}
		return
	}
	if m.enforced == enforced {
		return
	}
	m.enforced = enforced
	if enforced {
		m.logger.Info("访问鉴权已启用，/mcp 请求必须携带访问密钥")
		return
	}
	m.logger.Warn("访问鉴权已关闭，/mcp 不再校验访问密钥")
}

func (m *Middleware) reject(
	w http.ResponseWriter,
	r *http.Request,
	client, peer string,
	status int,
	event, reason string,
	retryAfter time.Duration,
	keyName string,
	cause error,
) {
	attrs := []any{
		"event", event,
		"remote", client,
		"peer", peer,
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"reason", reason,
	}
	if keyName != "" {
		attrs = append(attrs, "key_name", keyName)
	}
	if retryAfter > 0 {
		attrs = append(attrs, "retry_after", retryAfterSeconds(retryAfter))
	}
	if cause != nil {
		attrs = append(attrs, "err", cause)
	}
	if status >= http.StatusInternalServerError {
		m.logger.Error("鉴权服务不可用", attrs...)
	} else {
		m.logger.Warn("请求被拒绝", attrs...)
	}

	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="deepseek-search-fetch-mcp"`)
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter)))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    errorCode(status),
			"message": errorMessage(status),
		},
	})
}

func credentialFromRequest(r *http.Request) (string, string) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	apiKeyHeader := strings.TrimSpace(r.Header.Get("X-API-Key"))

	var bearer string
	if authHeader != "" {
		fields := strings.Fields(authHeader)
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			return "", "malformed_authorization"
		}
		bearer = strings.TrimSpace(fields[1])
	}

	switch {
	case bearer != "" && apiKeyHeader != "":
		if bearer != apiKeyHeader {
			return "", "conflicting_credentials"
		}
		return validateCredential(bearer)
	case bearer != "":
		return validateCredential(bearer)
	case apiKeyHeader != "":
		return validateCredential(apiKeyHeader)
	default:
		return "", "missing_key"
	}
}

func validateCredential(plain string) (string, string) {
	if plain == "" {
		return "", "missing_key"
	}
	if len(plain) > maxCredentialLen {
		return "", "key_too_long"
	}
	return plain, ""
}

func retryAfterSeconds(d time.Duration) int {
	seconds := int(math.Ceil(d.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

func errorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusTooManyRequests:
		return "rate_limited"
	default:
		return "auth_unavailable"
	}
}

func errorMessage(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "缺少或无效的访问密钥"
	case http.StatusTooManyRequests:
		return "请求过于频繁，请稍后重试"
	default:
		return "鉴权服务不可用"
	}
}
