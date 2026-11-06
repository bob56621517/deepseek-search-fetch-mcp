package auth

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	limiterIdleTTL       = 10 * time.Minute
	limiterCleanupEvery  = time.Minute
	limiterMaxEntries    = 10000
	limiterDefaultWindow = time.Minute
	limiterDefaultBlock  = 5 * time.Minute
)

// LimitConfig 是限流参数。0 表示关闭对应限流。
type LimitConfig struct {
	KeyRPS         float64
	KeyBurst       int
	AuthFailLimit  int
	AuthFailWindow time.Duration
	AuthFailBlock  time.Duration
	IPRPS          float64
	IPBurst        int
}

// Limiter 是进程内的访问密钥与客户端 IP 限流器。
// 多实例部署时每个实例独立计数。
type Limiter struct {
	cfg LimitConfig
	// now 可在测试中替换。
	now func() time.Time

	mu          sync.Mutex
	keyLimiters map[int64]*rateEntry
	ipLimiters  map[string]*rateEntry
	failStates  map[string]*failState
	lastCleanup time.Time
}

type rateEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type failState struct {
	count        int
	windowStart  time.Time
	blockedUntil time.Time
	lastSeen     time.Time
}

// NewLimiter 构建限流器，并对零值参数做安全兜底。
func NewLimiter(cfg LimitConfig) *Limiter {
	if cfg.KeyRPS < 0 {
		cfg.KeyRPS = 0
	}
	if cfg.KeyRPS > 0 && cfg.KeyBurst < 1 {
		cfg.KeyBurst = 1
	}
	if cfg.IPRPS < 0 {
		cfg.IPRPS = 0
	}
	if cfg.IPRPS > 0 && cfg.IPBurst < 1 {
		cfg.IPBurst = 1
	}
	if cfg.AuthFailLimit < 0 {
		cfg.AuthFailLimit = 0
	}
	if cfg.AuthFailLimit > 0 {
		if cfg.AuthFailWindow <= 0 {
			cfg.AuthFailWindow = limiterDefaultWindow
		}
		if cfg.AuthFailBlock <= 0 {
			cfg.AuthFailBlock = limiterDefaultBlock
		}
	}
	return &Limiter{
		cfg:         cfg,
		now:         time.Now,
		keyLimiters: make(map[int64]*rateEntry),
		ipLimiters:  make(map[string]*rateEntry),
		failStates:  make(map[string]*failState),
	}
}

// AllowKey 判断某个访问密钥是否还能继续请求。
func (l *Limiter) AllowKey(id int64) (bool, time.Duration) {
	if l == nil || l.cfg.KeyRPS <= 0 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeCleanupLocked(now)

	entry := l.keyLimiters[id]
	if entry == nil {
		entry = &rateEntry{limiter: rate.NewLimiter(rate.Limit(l.cfg.KeyRPS), l.cfg.KeyBurst)}
		l.keyLimiters[id] = entry
	}
	entry.lastSeen = now
	if entry.limiter.Allow() {
		return true, 0
	}
	return false, retryAfterFor(entry.limiter, l.cfg.KeyRPS)
}

// AllowIP 判断某个客户端 IP 是否还能继续请求。默认关闭。
func (l *Limiter) AllowIP(ip string) (bool, time.Duration) {
	if l == nil || l.cfg.IPRPS <= 0 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeCleanupLocked(now)

	entry := l.ipLimiters[ip]
	if entry == nil {
		entry = &rateEntry{limiter: rate.NewLimiter(rate.Limit(l.cfg.IPRPS), l.cfg.IPBurst)}
		l.ipLimiters[ip] = entry
	}
	entry.lastSeen = now
	if entry.limiter.Allow() {
		return true, 0
	}
	return false, retryAfterFor(entry.limiter, l.cfg.IPRPS)
}

// AuthBlocked 判断某个 IP 是否仍在鉴权失败封禁期内。
func (l *Limiter) AuthBlocked(ip string) (bool, time.Duration) {
	if l == nil || l.cfg.AuthFailLimit <= 0 {
		return false, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeCleanupLocked(now)

	state := l.failStates[ip]
	if state == nil || !now.Before(state.blockedUntil) {
		return false, 0
	}
	return true, state.blockedUntil.Sub(now)
}

// RecordAuthFailure 记录一次鉴权失败；达到阈值时开始封禁。
func (l *Limiter) RecordAuthFailure(ip string) {
	if l == nil || l.cfg.AuthFailLimit <= 0 {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeCleanupLocked(now)

	state := l.failStates[ip]
	if state == nil {
		state = &failState{windowStart: now}
		l.failStates[ip] = state
	}
	if now.Sub(state.windowStart) > l.cfg.AuthFailWindow {
		state.count = 0
		state.windowStart = now
		state.blockedUntil = time.Time{}
	}
	state.count++
	state.lastSeen = now
	if state.count >= l.cfg.AuthFailLimit {
		state.blockedUntil = now.Add(l.cfg.AuthFailBlock)
	}
}

// RecordAuthSuccess 清除某个 IP 的连续失败计数。
func (l *Limiter) RecordAuthSuccess(ip string) {
	if l == nil || l.cfg.AuthFailLimit <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failStates, ip)
}

func retryAfterFor(limiter *rate.Limiter, rps float64) time.Duration {
	if limiter == nil || rps <= 0 {
		return time.Second
	}
	tokens := limiter.Tokens()
	deficit := 1 - tokens
	if deficit <= 0 {
		return time.Second
	}
	delay := time.Duration(deficit / rps * float64(time.Second))
	if delay < time.Second {
		delay = time.Second
	}
	return delay
}

func (l *Limiter) maybeCleanupLocked(now time.Time) {
	if !l.lastCleanup.IsZero() && now.Sub(l.lastCleanup) < limiterCleanupEvery {
		return
	}
	l.lastCleanup = now
	for key, entry := range l.keyLimiters {
		if now.Sub(entry.lastSeen) > limiterIdleTTL {
			delete(l.keyLimiters, key)
		}
	}
	for ip, entry := range l.ipLimiters {
		if now.Sub(entry.lastSeen) > limiterIdleTTL {
			delete(l.ipLimiters, ip)
		}
	}
	for ip, state := range l.failStates {
		if now.Sub(state.lastSeen) > limiterIdleTTL && !now.Before(state.blockedUntil) {
			delete(l.failStates, ip)
		}
	}
	l.trimLocked()
}

// trimLocked 是内存安全阀：极端情况下直接丢弃部分条目，
// 避免伪造 IP 或随机密钥把 map 撑爆。限流状态本身允许丢失。
func (l *Limiter) trimLocked() {
	for len(l.keyLimiters) > limiterMaxEntries {
		for key := range l.keyLimiters {
			delete(l.keyLimiters, key)
			break
		}
	}
	for len(l.ipLimiters) > limiterMaxEntries {
		for ip := range l.ipLimiters {
			delete(l.ipLimiters, ip)
			break
		}
	}
	for len(l.failStates) > limiterMaxEntries {
		for ip := range l.failStates {
			delete(l.failStates, ip)
			break
		}
	}
}
