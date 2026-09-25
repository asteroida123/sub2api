//go:build unit

package tlsfingerprint

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSSProxyURL(t *testing.T) {
	// 合法：method:password@host:port
	u, err := url.Parse("ss://aes-256-gcm:secretpass@203.0.113.7:8388")
	require.NoError(t, err)
	method, password, addr, err := ParseSSProxyURL(u)
	require.NoError(t, err)
	require.Equal(t, "aes-256-gcm", method)
	require.Equal(t, "secretpass", password)
	require.Equal(t, "203.0.113.7:8388", addr)

	// 缺密码
	u, err = url.Parse("ss://aes-256-gcm@203.0.113.7:8388")
	require.NoError(t, err)
	_, _, _, err = ParseSSProxyURL(u)
	require.Error(t, err)

	// 缺端口
	u, err = url.Parse("ss://aes-256-gcm:pass@203.0.113.7")
	require.NoError(t, err)
	_, _, _, err = ParseSSProxyURL(u)
	require.Error(t, err)

	// nil
	_, _, _, err = ParseSSProxyURL(nil)
	require.Error(t, err)
}

func TestSSProxyDialerRejectsBadConfig(t *testing.T) {
	// 缺凭据：拨号前即失败
	u, err := url.Parse("ss://203.0.113.7:8388")
	require.NoError(t, err)
	dialer := NewSSProxyDialer(nil, u)
	_, err = dialer.DialTLSContext(context.Background(), "tcp", "chatgpt.com:443")
	require.Error(t, err)

	// 非 tcp 网络：直接拒绝
	u, err = url.Parse("ss://aes-256-gcm:pass@203.0.113.7:8388")
	require.NoError(t, err)
	dialer = NewSSProxyDialer(nil, u)
	_, err = dialer.DialTLSContext(context.Background(), "udp", "chatgpt.com:443")
	require.Error(t, err)

	// 不支持的加密方法：core.PickCipher 失败
	u, err = url.Parse("ss://not-a-cipher:pass@127.0.0.1:1")
	require.NoError(t, err)
	dialer = NewSSProxyDialer(nil, u)
	_, err = dialer.DialTLSContext(context.Background(), "tcp", "chatgpt.com:443")
	require.Error(t, err)
}
