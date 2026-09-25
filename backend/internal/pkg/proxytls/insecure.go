// Package proxytls 提供代理拨号的 TLS 校验策略工具。
//
// 实测背景（EXPERIMENT-LOG-292 §9）：IP 直连形式的代理节点其 TLS 证书通常没有
// IP SANs，Go 默认校验必失败（x509: cannot validate certificate for <IP>），
// 等价于 curl 的 --proxy-insecure。
package proxytls

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ApplyProxyHopInsecure 在 https 代理场景下，把"跳过代理跳证书校验"安装到 transport。
//
// 依赖 Go net/http 的既定语义：transport 带自定义 DialTLSContext 且使用 https 代理时，
// 代理 hop 会以【代理地址】回调 DialTLSContext（connectMethod.addr() 返回 proxyURL.Host），
// 目标 hop 则由 persistConn.addTLS 用 TLSClientConfig 完成常规校验。据此按 addr 区分两跳：
// 代理地址 → InsecureSkipVerify；目标地址 → 标准 ServerName 校验，不受豁免影响。
//
// enabled 为运行期开关（面板用户级配置），返回 false 时不做任何改装。
// 非扭转场景（nil 代理、非 https 代理）保持 transport 原样。
func ApplyProxyHopInsecure(transport *http.Transport, proxyURL *url.URL, enabled func() bool) {
	if transport == nil || proxyURL == nil || !strings.EqualFold(proxyURL.Scheme, "https") {
		return
	}
	if enabled != nil && !enabled() {
		return
	}
	proxyAddr := canonicalProxyAddr(proxyURL)
	proxyHost := proxyURL.Hostname()
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		cfg := &tls.Config{
			NextProtos: []string{"h2", "http/1.1"},
		}
		if sameHostPort(addr, proxyAddr) {
			// 代理 hop：用户显式豁免证书校验
			cfg.InsecureSkipVerify = true //nolint:gosec // 用户级显式开关，仅作用于代理一跳
			cfg.ServerName = proxyHost
		} else {
			// 目标 hop：常规校验
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			cfg.ServerName = host
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
}

func canonicalProxyAddr(proxyURL *url.URL) string {
	host := proxyURL.Host
	if proxyURL.Port() == "" {
		if strings.EqualFold(proxyURL.Scheme, "https") {
			host = net.JoinHostPort(proxyURL.Hostname(), "443")
		} else {
			host = net.JoinHostPort(proxyURL.Hostname(), "80")
		}
	}
	return host
}

func sameHostPort(a, b string) bool {
	if a == b {
		return true
	}
	ha, pa, errA := net.SplitHostPort(a)
	hb, pb, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil {
		return false
	}
	if !strings.EqualFold(ha, hb) {
		return false
	}
	if pa == pb {
		return true
	}
	// 缺省端口归一（80/443）
	return (pa == "443" && pb == "") || (pa == "" && pb == "443") ||
		(pa == "80" && pb == "") || (pa == "" && pb == "80")
}
