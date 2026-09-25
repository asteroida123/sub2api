package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SettingKeyProxyDialSettings settings 表中代理拨号配置的 key。
const SettingKeyProxyDialSettings = "proxy_dial_settings"

// ProxyDialSettings 代理拨号层用户级配置。
//
// InsecureSkipVerify 的实测背景（EXPERIMENT-LOG-292 §9）：IP 直连形式的代理节点其
// TLS 证书通常没有 IP SANs，Go 默认校验必失败（x509: cannot validate certificate
// for <IP>），等价于 curl 的 --proxy-insecure。开启后仅跳过【到代理这一跳】的证书
// 校验，到上游目标的 TLS 校验不受影响。
type ProxyDialSettings struct {
	InsecureSkipVerify bool `json:"insecure_skip_verify"`
}

// DefaultProxyDialSettings 默认关闭跳过校验（安全默认）。
func DefaultProxyDialSettings() *ProxyDialSettings {
	return &ProxyDialSettings{InsecureSkipVerify: false}
}

// GetProxyDialSettings 读取代理拨号配置；未配置或损坏时回退默认值。
func (s *SettingService) GetProxyDialSettings(ctx context.Context) (*ProxyDialSettings, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultProxyDialSettings(), nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyProxyDialSettings)
	if err != nil {
		if err == ErrSettingNotFound {
			return DefaultProxyDialSettings(), nil
		}
		return nil, fmt.Errorf("get proxy dial settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return DefaultProxyDialSettings(), nil
	}
	var settings ProxyDialSettings
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return DefaultProxyDialSettings(), nil
	}
	return &settings, nil
}

// SetProxyDialSettings 写回代理拨号配置。
func (s *SettingService) SetProxyDialSettings(ctx context.Context, settings *ProxyDialSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository unavailable")
	}
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal proxy dial settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyProxyDialSettings, string(data))
}

// proxyDialSettingsProvider 是带 60s TTL 的共享配置缓存。
// 代理拨号发生在热路径（HTTP 上游 Transport 缓存键、WS 代理客户端缓存键、探针等），
// 不能每次拨号都读 settings 存储；绑定共享实例后各拨号层统一走 SharedProxyTLSInsecureSkipVerify。
type proxyDialSettingsProvider struct {
	mu        sync.Mutex
	service   *SettingService
	cached    ProxyDialSettings
	expiresAt time.Time
}

const proxyDialSettingsCacheTTL = 60 * time.Second

func (p *proxyDialSettingsProvider) bind(service *SettingService) {
	if p == nil || service == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.service = service
	p.expiresAt = time.Time{} // 服务变更后强制刷新
}

func (p *proxyDialSettingsProvider) insecureSkipVerify(ctx context.Context) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.service != nil && now.Before(p.expiresAt) {
		return p.cached.InsecureSkipVerify
	}
	settings := DefaultProxyDialSettings()
	if p.service != nil {
		if fetched, err := p.service.GetProxyDialSettings(ctx); err == nil && fetched != nil {
			settings = fetched
		}
	}
	p.cached = *settings
	p.expiresAt = now.Add(proxyDialSettingsCacheTTL)
	return p.cached.InsecureSkipVerify
}

// sharedProxyDialSettings 应用级共享实例（单例 SettingService 的伴生缓存）。
var sharedProxyDialSettings = &proxyDialSettingsProvider{}

// BindSharedSettingService 在构造 SettingService 时绑定共享代理拨号配置缓存。
func BindSharedSettingService(service *SettingService) {
	sharedProxyDialSettings.bind(service)
}

// SharedProxyTLSInsecureSkipVerify 供各拨号层读取"跳过代理跳证书校验"开关。
func SharedProxyTLSInsecureSkipVerify(ctx context.Context) bool {
	return sharedProxyDialSettings.insecureSkipVerify(ctx)
}
