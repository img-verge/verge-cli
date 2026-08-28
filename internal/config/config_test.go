package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// useTempConfig points Path() at a throwaway file. 每个测试都必须调用它，否则会读写
// 开发者本机真实的 config.json。
func useTempConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verge", "config.json")
	t.Setenv("VERGE_CONFIG", path)
	return path
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	useTempConfig(t)
	file, err := Load()
	if err != nil {
		t.Fatalf("Load with no config file should succeed: %v", err)
	}
	if file != (File{}) {
		t.Errorf("Load = %+v, want the zero value", file)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := useTempConfig(t)
	want := File{
		APIKey:      "sk-verge-1234567890",
		BaseURL:     "https://api.verge-ai.xyz/v1",
		Model:       "gpt-image-2",
		Resolution:  "2k",
		AspectRatio: "16:9",
	}
	written, err := Save(want)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if written != path {
		t.Errorf("Save returned %q, want %q", written, path)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}

	// 落盘内容必须是 snake_case JSON：用户会手改这个文件。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("config file is not JSON: %v\n%s", err, raw)
	}
	for _, key := range []string{"api_key", "base_url", "model", "resolution", "aspect_ratio"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("config file is missing %q: %s", key, raw)
		}
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("config file should end with a newline so shells and editors behave")
	}
	if runtime.GOOS != "windows" {
		stat, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		// 文件里有明文 API Key，权限必须是 0600。
		if mode := stat.Mode().Perm(); mode != 0o600 {
			t.Errorf("config file mode = %04o, want 0600: it stores a plaintext API key", mode)
		}
	}
}

func TestSaveOverwritesExistingConfig(t *testing.T) {
	useTempConfig(t)
	if _, err := Save(File{APIKey: "sk-old", Model: "gpt-image-2"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Save(File{APIKey: "sk-new"}); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Save 写的是整份配置，被清空的字段不该残留上一次的值。
	if got.APIKey != "sk-new" || got.Model != "" {
		t.Errorf("Load = %+v, want only the new key", got)
	}
}

func TestLoadReportsCorruptConfig(t *testing.T) {
	path := useTempConfig(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 静默当成空配置会让用户以为 Key 没保存成功，必须报错并指出文件路径。
	if _, err := Load(); err == nil {
		t.Fatal("Load should report a corrupt config instead of silently ignoring it")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name %q", err, path)
	}
}

func TestPathHonoursOverride(t *testing.T) {
	path := useTempConfig(t)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if got != path {
		t.Errorf("Path = %q, want the VERGE_CONFIG override %q", got, path)
	}
}

func TestPathFollowsXDGWhenSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows resolves %APPDATA% before XDG_CONFIG_HOME")
	}
	t.Setenv("VERGE_CONFIG", "")
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join(dir, "verge", "config.json"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestResolvePrecedence(t *testing.T) {
	defaults := Resolved{
		BaseURL:     "https://api.verge-ai.xyz/v1",
		Model:       "gpt-image-2",
		Resolution:  "1080p",
		AspectRatio: "1:1",
	}
	file := File{
		APIKey:      "sk-file",
		BaseURL:     "https://file.example.com/v1",
		Model:       "gemini-3-pro-image-preview",
		Resolution:  "4k",
		AspectRatio: "16:9",
	}

	t.Run("flags win over everything", func(t *testing.T) {
		t.Setenv(EnvAPIKey, "sk-env")
		t.Setenv(EnvBaseURL, "https://env.example.com/v1")
		got := Resolve(file, Overrides{APIKey: "sk-flag", BaseURL: "https://flag.example.com/v1"}, defaults)
		if got.APIKey != "sk-flag" || got.APIKeySource != "--api-key" {
			t.Errorf("APIKey = %q from %q, want sk-flag from --api-key", got.APIKey, got.APIKeySource)
		}
		if got.BaseURL != "https://flag.example.com/v1" {
			t.Errorf("BaseURL = %q, want the flag value", got.BaseURL)
		}
	})

	t.Run("env wins over the file", func(t *testing.T) {
		t.Setenv(EnvAPIKey, "sk-env")
		t.Setenv(EnvBaseURL, "https://env.example.com/v1")
		got := Resolve(file, Overrides{}, defaults)
		if got.APIKey != "sk-env" || got.APIKeySource != EnvAPIKey {
			t.Errorf("APIKey = %q from %q, want sk-env from %s", got.APIKey, got.APIKeySource, EnvAPIKey)
		}
		if got.BaseURL != "https://env.example.com/v1" {
			t.Errorf("BaseURL = %q, want the env value", got.BaseURL)
		}
	})

	t.Run("the file wins over defaults", func(t *testing.T) {
		t.Setenv(EnvAPIKey, "")
		t.Setenv(EnvBaseURL, "")
		got := Resolve(file, Overrides{}, defaults)
		if got.APIKey != "sk-file" || got.APIKeySource != "config file" {
			t.Errorf("APIKey = %q from %q, want sk-file from the config file", got.APIKey, got.APIKeySource)
		}
		if got.BaseURL != file.BaseURL {
			t.Errorf("BaseURL = %q, want %q", got.BaseURL, file.BaseURL)
		}
		if got.Model != file.Model || got.Resolution != file.Resolution || got.AspectRatio != file.AspectRatio {
			t.Errorf("image task defaults = %+v, want the file values", got)
		}
	})

	t.Run("defaults fill the gaps", func(t *testing.T) {
		t.Setenv(EnvAPIKey, "")
		t.Setenv(EnvBaseURL, "")
		got := Resolve(File{}, Overrides{}, defaults)
		if got.APIKey != "" || got.APIKeySource != "" {
			t.Errorf("APIKey = %q from %q, want an empty key so the caller can prompt for login", got.APIKey, got.APIKeySource)
		}
		if got.BaseURL != defaults.BaseURL || got.Model != defaults.Model {
			t.Errorf("resolved = %+v, want the built-in defaults", got)
		}
	})

	t.Run("whitespace-only values are ignored", func(t *testing.T) {
		// 复制粘贴 Key 时很容易带上换行，"   " 不能被当成有效凭证。
		t.Setenv(EnvAPIKey, "   ")
		t.Setenv(EnvBaseURL, " ")
		got := Resolve(File{APIKey: "\tsk-file\n"}, Overrides{APIKey: "  "}, defaults)
		if got.APIKey != "sk-file" || got.APIKeySource != "config file" {
			t.Errorf("APIKey = %q from %q, want the trimmed file key", got.APIKey, got.APIKeySource)
		}
		if got.BaseURL != defaults.BaseURL {
			t.Errorf("BaseURL = %q, want the default", got.BaseURL)
		}
	})
}

func TestMaskKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "a long key keeps both ends", in: "sk-verge-abcdef123456", want: "sk-v******3456"},
		// 短 Key 整体打码：留头尾就等于泄露了大半。
		{name: "a 12 char key is fully masked", in: "sk-abcdefghi", want: "************"},
		{name: "a short key is fully masked", in: "sk-1", want: "****"},
		{name: "surrounding whitespace is trimmed first", in: "  sk-verge-abcdef123456  ", want: "sk-v******3456"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := MaskKey(test.in)
			if got != test.want {
				t.Errorf("MaskKey(%q) = %q, want %q", test.in, got, test.want)
			}
			trimmed := strings.TrimSpace(test.in)
			if len(trimmed) > 12 && strings.Contains(got, trimmed) {
				t.Errorf("MaskKey(%q) leaked the whole key", test.in)
			}
		})
	}
}
