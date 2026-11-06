package auth

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIPResolver 根据 trusted_proxies 解析真实客户端 IP。
// 未配置可信代理时不读取 X-Forwarded-For，避免客户端伪造。
type ClientIPResolver struct {
	trusted []netip.Prefix
}

// NewClientIPResolver 解析 trusted_proxies 配置。条目可以是 CIDR，也可以是单个 IP。
func NewClientIPResolver(raw []string) (*ClientIPResolver, error) {
	resolver := &ClientIPResolver{}
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(item); err == nil {
			resolver.trusted = append(resolver.trusted, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("无效的 trusted_proxies 条目 %q: %w", item, err)
		}
		addr = addr.Unmap()
		bits := 128
		if addr.Is4() {
			bits = 32
		}
		resolver.trusted = append(resolver.trusted, netip.PrefixFrom(addr, bits))
	}
	return resolver, nil
}

// Resolve 返回真实客户端 IP 与直接对端 IP。
// peer 是 TCP 对端，remote 是经过可信代理链解析后的客户端地址。
func (r *ClientIPResolver) Resolve(req *http.Request) (remote, peer string) {
	peerAddr, ok := parseRemoteAddr(req.RemoteAddr)
	if !ok {
		return "unknown", "unknown"
	}
	peer = peerAddr.String()
	remote = peer
	if r == nil || len(r.trusted) == 0 {
		return remote, peer
	}
	if !r.isTrusted(peerAddr) {
		return remote, peer
	}

	raw := strings.TrimSpace(strings.Join(req.Header.Values("X-Forwarded-For"), ","))
	if raw == "" {
		return remote, peer
	}
	parts := strings.Split(raw, ",")
	forwarded := make([]netip.Addr, 0, len(parts))
	for _, part := range parts {
		addr, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil {
			// 任何一项不合法都忽略整个 XFF，避免部分信任被利用。
			return peer, peer
		}
		forwarded = append(forwarded, addr.Unmap())
	}
	if len(forwarded) == 0 {
		return remote, peer
	}

	// 从右往左跳过可信代理，取最右侧的不可信地址。
	current := forwarded[len(forwarded)-1]
	for i := len(forwarded) - 2; i >= 0; i-- {
		if !r.isTrusted(current) {
			break
		}
		current = forwarded[i]
	}
	return current.String(), peer
}

func (r *ClientIPResolver) isTrusted(addr netip.Addr) bool {
	for _, prefix := range r.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func parseRemoteAddr(raw string) (netip.Addr, bool) {
	if addrPort, err := netip.ParseAddrPort(raw); err == nil {
		return addrPort.Addr().Unmap(), true
	}
	if addr, err := netip.ParseAddr(raw); err == nil {
		return addr.Unmap(), true
	}
	return netip.Addr{}, false
}
