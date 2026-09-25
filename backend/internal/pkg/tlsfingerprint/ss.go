// ss.go 提供原生 Shadowsocks 代理拨号（窗口猎手 P3）。
//
// 约定（与 proxies 表字段映射）：
//   - protocol = "ss"
//   - username = 加密方法（如 aes-256-gcm / chacha20-ietf-poly1305）
//   - password = 密码
//   - host:port = ss 服务器地址（端口必填）
//
// 隧道语义：TCP 连上 ss 服务器 → cipher.StreamConn 加密包装 → 写入目标地址
// （SOCKS5 地址格式，ss 协议标准载荷），之后该连接等价于到目标的明文 TCP。
package tlsfingerprint

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

// SSProxyDialer 通过 Shadowsocks 代理建立带 utls 指纹的 TLS 连接。
type SSProxyDialer struct {
	profile  *Profile
	proxyURL *url.URL
}

// NewSSProxyDialer 创建 ss 隧道 + TLS 指纹拨号器。
func NewSSProxyDialer(profile *Profile, proxyURL *url.URL) *SSProxyDialer {
	return &SSProxyDialer{profile: profile, proxyURL: proxyURL}
}

// ParseSSProxyURL 校验并规范化 ss 代理 URL：要求显式端口与 method:password 凭据。
func ParseSSProxyURL(proxyURL *url.URL) (method, password, addr string, err error) {
	if proxyURL == nil {
		return "", "", "", fmt.Errorf("ss proxy url is nil")
	}
	method = proxyURL.User.Username()
	password, _ = proxyURL.User.Password()
	if strings.TrimSpace(method) == "" || strings.TrimSpace(password) == "" {
		return "", "", "", fmt.Errorf("ss proxy requires username=method and password")
	}
	if proxyURL.Port() == "" {
		return "", "", "", fmt.Errorf("ss proxy requires explicit port")
	}
	addr = proxyURL.Host
	return method, password, addr, nil
}

// DialSSContext 拨通 ss 隧道，返回已指向目标 addr 的明文连接。
// 作为 http.Transport 的 DialContext 使用时，到 https 目标的 TLS 由 Transport 自行完成
// （走 ss 隧道，指纹为标准库；需要指纹时使用 SSProxyDialer.DialTLSContext）。
func DialSSContext(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	method, password, proxyAddr, err := ParseSSProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	cipher, err := core.PickCipher(method, nil, password)
	if err != nil {
		return nil, fmt.Errorf("ss cipher %q: %w", method, err)
	}
	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to ss server: %w", err)
	}
	conn := cipher.StreamConn(raw)
	target := socks.ParseAddr(addr)
	if target == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ss: invalid target address %q", addr)
	}
	if _, err := conn.Write(target); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ss: write target: %w", err)
	}
	return conn, nil
}

// DialTLSContext 通过 ss 隧道对目标完成 utls 指纹 TLS 握手。
// 用作 http.Transport 的 DialTLSContext（buildUpstreamTransportWithTLSFingerprint 的 ss 分支）。
func (d *SSProxyDialer) DialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("ss dialer only supports tcp, got %q", network)
	}
	conn, err := DialSSContext(ctx, d.proxyURL, addr)
	if err != nil {
		return nil, err
	}
	return performTLSHandshake(ctx, conn, d.profile, addr)
}
