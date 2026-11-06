package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPResolver(t *testing.T) {
	cases := []struct {
		name       string
		trusted    []string
		remoteAddr string
		xff        []string
		wantRemote string
		wantPeer   string
	}{
		{
			name:       "未配置可信代理时忽略 XFF",
			remoteAddr: "203.0.113.5:1234",
			xff:        []string{"1.2.3.4"},
			wantRemote: "203.0.113.5",
			wantPeer:   "203.0.113.5",
		},
		{
			name:       "对端不可信时忽略 XFF",
			trusted:    []string{"127.0.0.1"},
			remoteAddr: "203.0.113.5:1234",
			xff:        []string{"1.2.3.4"},
			wantRemote: "203.0.113.5",
			wantPeer:   "203.0.113.5",
		},
		{
			name:       "可信代理取最右侧不可信地址",
			trusted:    []string{"127.0.0.1", "10.0.0.0/8"},
			remoteAddr: "127.0.0.1:1234",
			xff:        []string{"1.2.3.4, 10.0.0.1"},
			wantRemote: "1.2.3.4",
			wantPeer:   "127.0.0.1",
		},
		{
			name:       "单个 XFF",
			trusted:    []string{"127.0.0.1"},
			remoteAddr: "127.0.0.1:1234",
			xff:        []string{"1.2.3.4"},
			wantRemote: "1.2.3.4",
			wantPeer:   "127.0.0.1",
		},
		{
			name:       "多个 XFF 头合并",
			trusted:    []string{"127.0.0.1", "10.0.0.0/8"},
			remoteAddr: "127.0.0.1:1234",
			xff:        []string{"1.2.3.4", "10.0.0.1"},
			wantRemote: "1.2.3.4",
			wantPeer:   "127.0.0.1",
		},
		{
			name:       "非法 XFF 回退到对端",
			trusted:    []string{"127.0.0.1"},
			remoteAddr: "127.0.0.1:1234",
			xff:        []string{"1.2.3.4, nope"},
			wantRemote: "127.0.0.1",
			wantPeer:   "127.0.0.1",
		},
		{
			name:       "IPv6",
			trusted:    []string{"::1"},
			remoteAddr: "[::1]:1234",
			xff:        []string{"2001:db8::1"},
			wantRemote: "2001:db8::1",
			wantPeer:   "::1",
		},
		{
			name:       "无法解析 RemoteAddr",
			remoteAddr: "not-an-address",
			wantRemote: "unknown",
			wantPeer:   "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver, err := NewClientIPResolver(tc.trusted)
			if err != nil {
				t.Fatalf("解析可信代理失败: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.RemoteAddr = tc.remoteAddr
			for _, value := range tc.xff {
				req.Header.Add("X-Forwarded-For", value)
			}
			remote, peer := resolver.Resolve(req)
			if remote != tc.wantRemote || peer != tc.wantPeer {
				t.Fatalf("Resolve = (%q, %q), want (%q, %q)", remote, peer, tc.wantRemote, tc.wantPeer)
			}
		})
	}
}

func TestNewClientIPResolverInvalid(t *testing.T) {
	if _, err := NewClientIPResolver([]string{"not-a-cidr"}); err == nil {
		t.Fatal("非法 trusted_proxies 条目应报错")
	}
}

func TestNewClientIPResolverSkipsEmpty(t *testing.T) {
	resolver, err := NewClientIPResolver([]string{"", "  ", "127.0.0.1"})
	if err != nil {
		t.Fatalf("空条目应被忽略: %v", err)
	}
	if len(resolver.trusted) != 1 {
		t.Fatalf("可信代理数量 = %d, want 1", len(resolver.trusted))
	}
}
