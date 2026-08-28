// Package config resolves verge-cli settings from flags, environment and a config file.
//
// 优先级固定为 flag > 环境变量 > 配置文件 > 内置默认值。API Key 落盘时权限收到 0600，
// 并且 show 命令只显示掩码后的值。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Environment variables recognised by the CLI.
const (
	EnvAPIKey  = "VERGE_API_KEY"
	EnvBaseURL = "VERGE_API_BASE_URL"
)

// File is the on-disk config shape. 只存长期有效的偏好，不存任务态。
type File struct {
	APIKey      string `json:"api_key,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	Model       string `json:"model,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

// Path returns the config file location for this OS.
//
// Windows 用 %APPDATA%\verge\config.json，其余平台遵循 XDG。
func Path() (string, error) {
	if override := strings.TrimSpace(os.Getenv("VERGE_CONFIG")); override != "" {
		return override, nil
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "verge", "config.json"), nil
		}
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "verge", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "verge", "config.json"), nil
}

// Load reads the config file. 文件不存在返回零值而不是错误 —— 没配过文件是正常状态。
func Load() (File, error) {
	path, err := Path()
	if err != nil {
		return File{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, nil
		}
		return File{}, fmt.Errorf("read %s: %w", path, err)
	}
	var file File
	if err := json.Unmarshal(raw, &file); err != nil {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return file, nil
}

// Save writes the config file with 0600 permissions.
//
// 先写临时文件再 rename：中途失败不会把已有配置截断成半个文件。
func Save(file File) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode config: %w", err)
	}
	raw = append(raw, '\n')

	temp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return "", fmt.Errorf("create temp config: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if err := temp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		temp.Close()
		return "", fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return "", fmt.Errorf("write temp config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temp config: %w", err)
	}
	// Windows 上 rename 到已存在的路径会失败，先删旧文件。
	if runtime.GOOS == "windows" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("replace %s: %w", path, err)
		}
	}
	if err := os.Rename(tempName, path); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// Resolved is the effective configuration after applying precedence.
type Resolved struct {
	APIKey      string
	BaseURL     string
	Model       string
	Resolution  string
	AspectRatio string
	// APIKeySource names where the key came from, for diagnostics.
	APIKeySource string
}

// Overrides carries values that came from command-line flags. 空串表示未指定。
type Overrides struct {
	APIKey  string
	BaseURL string
}

// Resolve applies flag > env > file > default, and reports where the key came from.
func Resolve(file File, flags Overrides, defaults Resolved) Resolved {
	out := Resolved{
		Model:       firstNonEmpty(file.Model, defaults.Model),
		Resolution:  firstNonEmpty(file.Resolution, defaults.Resolution),
		AspectRatio: firstNonEmpty(file.AspectRatio, defaults.AspectRatio),
	}
	switch {
	case strings.TrimSpace(flags.APIKey) != "":
		out.APIKey = strings.TrimSpace(flags.APIKey)
		out.APIKeySource = "--api-key"
	case strings.TrimSpace(os.Getenv(EnvAPIKey)) != "":
		out.APIKey = strings.TrimSpace(os.Getenv(EnvAPIKey))
		out.APIKeySource = EnvAPIKey
	case strings.TrimSpace(file.APIKey) != "":
		out.APIKey = strings.TrimSpace(file.APIKey)
		out.APIKeySource = "config file"
	}
	out.BaseURL = firstNonEmpty(
		strings.TrimSpace(flags.BaseURL),
		strings.TrimSpace(os.Getenv(EnvBaseURL)),
		strings.TrimSpace(file.BaseURL),
		defaults.BaseURL,
	)
	return out
}

// MaskKey renders an API key safe to print. 保留头尾各 4 位，长度不足就整体打码，
// 避免短 Key 被完整泄露到日志里。
func MaskKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 12 {
		return strings.Repeat("*", len(trimmed))
	}
	return trimmed[:4] + strings.Repeat("*", 6) + trimmed[len(trimmed)-4:]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
