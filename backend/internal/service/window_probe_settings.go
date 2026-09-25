package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SettingKeyWindowProbeSettings settings 表中窗口探针配置的 key。
const SettingKeyWindowProbeSettings = "window_probe_settings"

// GetWindowProbeSettings 获取窗口探针配置；未配置或配置损坏时回退默认值。
func (s *SettingService) GetWindowProbeSettings(ctx context.Context) (*WindowProbeSettings, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultWindowProbeSettings(), nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyWindowProbeSettings)
	if err != nil {
		if err == ErrSettingNotFound {
			return DefaultWindowProbeSettings(), nil
		}
		return nil, fmt.Errorf("get window probe settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return DefaultWindowProbeSettings(), nil
	}
	var settings WindowProbeSettings
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return DefaultWindowProbeSettings(), nil
	}
	settings.Normalize()
	return &settings, nil
}

// SetWindowProbeSettings 校验并写回窗口探针配置。
func (s *SettingService) SetWindowProbeSettings(ctx context.Context, settings *WindowProbeSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository unavailable")
	}
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	settings.Normalize()
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal window probe settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyWindowProbeSettings, string(data))
}
