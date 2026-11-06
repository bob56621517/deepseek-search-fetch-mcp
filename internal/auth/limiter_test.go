package auth

import (
	"testing"
	"time"
)

func newTestLimiter(cfg LimitConfig, now *time.Time) *Limiter {
	limiter := NewLimiter(cfg)
	limiter.now = func() time.Time { return *now }
	return limiter
}

func TestLimiterKeyRate(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(LimitConfig{KeyRPS: 1, KeyBurst: 1}, &now)

	if ok, _ := limiter.AllowKey(1); !ok {
		t.Fatal("首个请求应放行")
	}
	ok, retry := limiter.AllowKey(1)
	if ok {
		t.Fatal("burst 耗尽后应拒绝")
	}
	if retry < time.Second {
		t.Fatalf("Retry-After 至少应为 1s，实际 %s", retry)
	}
	if ok, _ := limiter.AllowKey(2); !ok {
		t.Fatal("不同 key 应独立计数")
	}
	if ok, _ := limiter.AllowKey(1); ok {
		t.Fatal("被限流的 key 不应影响其他 key 后再次放行")
	}
}

func TestLimiterIPRate(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(LimitConfig{IPRPS: 1, IPBurst: 1}, &now)

	if ok, _ := limiter.AllowIP("1.2.3.4"); !ok {
		t.Fatal("首个请求应放行")
	}
	if ok, _ := limiter.AllowIP("1.2.3.4"); ok {
		t.Fatal("burst 耗尽后应拒绝")
	}
	if ok, _ := limiter.AllowIP("5.6.7.8"); !ok {
		t.Fatal("不同 IP 应独立计数")
	}
}

func TestLimiterAuthFailureBlock(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(LimitConfig{
		AuthFailLimit:  2,
		AuthFailWindow: time.Minute,
		AuthFailBlock:  5 * time.Minute,
	}, &now)

	if blocked, _ := limiter.AuthBlocked("1.2.3.4"); blocked {
		t.Fatal("初始不应封禁")
	}
	limiter.RecordAuthFailure("1.2.3.4")
	if blocked, _ := limiter.AuthBlocked("1.2.3.4"); blocked {
		t.Fatal("第 1 次失败后不应封禁")
	}
	limiter.RecordAuthFailure("1.2.3.4")
	blocked, retry := limiter.AuthBlocked("1.2.3.4")
	if !blocked {
		t.Fatal("达到阈值后应封禁")
	}
	if retry != 5*time.Minute {
		t.Fatalf("封禁剩余时间 = %s, want 5m", retry)
	}

	now = now.Add(5 * time.Minute)
	if blocked, _ := limiter.AuthBlocked("1.2.3.4"); blocked {
		t.Fatal("封禁到期后应放行")
	}
}

func TestLimiterAuthFailureWindowReset(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(LimitConfig{
		AuthFailLimit:  2,
		AuthFailWindow: time.Minute,
		AuthFailBlock:  5 * time.Minute,
	}, &now)

	limiter.RecordAuthFailure("1.2.3.4")
	now = now.Add(61 * time.Second)
	limiter.RecordAuthFailure("1.2.3.4")
	if blocked, _ := limiter.AuthBlocked("1.2.3.4"); blocked {
		t.Fatal("窗口过期后计数应重置，不应封禁")
	}
}

func TestLimiterAuthSuccessResets(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(LimitConfig{
		AuthFailLimit:  2,
		AuthFailWindow: time.Minute,
		AuthFailBlock:  5 * time.Minute,
	}, &now)

	limiter.RecordAuthFailure("1.2.3.4")
	limiter.RecordAuthSuccess("1.2.3.4")
	limiter.RecordAuthFailure("1.2.3.4")
	if blocked, _ := limiter.AuthBlocked("1.2.3.4"); blocked {
		t.Fatal("成功一次后失败计数应清零")
	}
}

func TestLimiterCleanup(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(LimitConfig{
		KeyRPS:        1,
		KeyBurst:      1,
		IPRPS:         1,
		IPBurst:       1,
		AuthFailLimit: 2,
	}, &now)

	limiter.AllowKey(1)
	limiter.AllowIP("1.2.3.4")
	limiter.RecordAuthFailure("5.6.7.8")
	if len(limiter.keyLimiters) != 1 || len(limiter.ipLimiters) != 1 || len(limiter.failStates) != 1 {
		t.Fatal("测试前置状态不符")
	}

	now = now.Add(limiterIdleTTL + time.Minute)
	limiter.AllowKey(2)
	if len(limiter.keyLimiters) != 1 {
		t.Fatalf("过期 key limiter 应被清理，实际 %d", len(limiter.keyLimiters))
	}
	if len(limiter.ipLimiters) != 0 {
		t.Fatalf("过期 IP limiter 应被清理，实际 %d", len(limiter.ipLimiters))
	}
	if len(limiter.failStates) != 0 {
		t.Fatalf("过期失败状态应被清理，实际 %d", len(limiter.failStates))
	}
}
