package proxyutil

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// ss 代理（窗口猎手 P3）：走自定义 DialContext（ss 隧道），不设置 Transport.Proxy。
func TestConfigureTransportProxySS(t *testing.T) {
	proxyURL, err := url.Parse("ss://aes-256-gcm:pass@203.0.113.7:8388")
	require.NoError(t, err)

	transport := &http.Transport{}
	require.NoError(t, ConfigureTransportProxy(transport, proxyURL))
	require.Nil(t, transport.Proxy, "ss 代理不应设置 Transport.Proxy")
	require.NotNil(t, transport.DialContext, "ss 代理应安装隧道 DialContext")

	// 未配置代理时不动 DialContext
	transport2 := &http.Transport{}
	require.NoError(t, ConfigureTransportProxy(transport2, nil))
	require.Nil(t, transport2.DialContext)
	require.Nil(t, transport2.Proxy)
}
