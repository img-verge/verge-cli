package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/img-verge/verge-cli/internal/config"
	"github.com/img-verge/verge-cli/internal/vergeapi"
)

const usageConfig = `usage: verge-cli config <subcommand> [args]

Read and write the local config file. The file only holds long-lived preferences; it
stores no task state.

subcommands:
  show                 print the effective settings, with the API key masked
  path                 print the config file location
  set <key> <value>    write one setting
  unset <key>          remove one setting

keys:
  api-key       API key; the file is written with 0600 permissions
  base-url      API base URL; /v1 is appended when missing
  model         default model id
  resolution    default resolution
  aspect-ratio  default aspect ratio

Precedence is flag > environment variable > config file > built-in default, so
$VERGE_API_KEY still wins over a stored key.

examples:
  verge config set api-key sk-...
  verge config set model gemini-3-pro-image-preview
  verge config show
  verge config unset base-url
`

func runConfig(e *env, args []string) error {
	fs := e.newFlagSet("config")
	if err := e.parse(fs, args, usageConfig); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageErrorf(usageConfig, "expected a subcommand: show, path, set or unset")
	}

	switch rest[0] {
	case "show":
		if len(rest) > 1 {
			return usageErrorf(usageConfig, "config show takes no arguments")
		}
		return e.configShow()
	case "path":
		if len(rest) > 1 {
			return usageErrorf(usageConfig, "config path takes no arguments")
		}
		path, err := config.Path()
		if err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, path)
		return nil
	case "set":
		if len(rest) != 3 {
			return usageErrorf(usageConfig, "config set needs a key and a value")
		}
		return e.configSet(rest[1], rest[2])
	case "unset":
		if len(rest) != 2 {
			return usageErrorf(usageConfig, "config unset needs exactly one key")
		}
		return e.configSet(rest[1], "")
	default:
		return usageErrorf(usageConfig, "unknown config subcommand %q", rest[0])
	}
}

// configView is the --json shape of `verge-cli config show`.
//
// APIKey 恒为掩码后的值：这个命令的输出经常被贴进 issue 和聊天窗口。
type configView struct {
	ConfigFile       string `json:"config_file"`
	ConfigFileExists bool   `json:"config_file_exists"`
	APIKey           string `json:"api_key"`
	APIKeySource     string `json:"api_key_source"`
	BaseURL          string `json:"base_url"`
	Model            string `json:"model"`
	Resolution       string `json:"resolution"`
	AspectRatio      string `json:"aspect_ratio"`
}

func (e *env) configShow() error {
	if _, err := e.resolveConfig(); err != nil {
		return err
	}
	path, err := config.Path()
	if err != nil {
		return err
	}
	_, statErr := os.Stat(path)

	view := configView{
		ConfigFile:       path,
		ConfigFileExists: statErr == nil,
		APIKey:           config.MaskKey(e.cfg.APIKey),
		APIKeySource:     e.cfg.APIKeySource,
		BaseURL:          e.cfg.BaseURL,
		Model:            e.cfg.Model,
		Resolution:       e.cfg.Resolution,
		AspectRatio:      e.cfg.AspectRatio,
	}
	// 展示实际会用到的地址，而不是用户填的原样：/v1 是这里补上的，不展示出来
	// 排查 404 时很容易白绕一圈。
	if normalized, err := vergeapi.NormalizeBaseURL(view.BaseURL); err == nil {
		view.BaseURL = normalized
	}
	if view.APIKey == "" {
		view.APIKey = "(not set)"
		view.APIKeySource = "none"
	}

	if e.global.jsonOut {
		raw, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			return fmt.Errorf("encode config: %w", err)
		}
		fmt.Fprintln(e.stdout, string(raw))
		return nil
	}

	table, buf := newTable(0)
	exists := "does not exist yet"
	if view.ConfigFileExists {
		exists = "exists"
	}
	fmt.Fprintf(table, "config file\t%s (%s)\n", view.ConfigFile, exists)
	source := view.APIKeySource
	if source != "" && source != "none" {
		source = " (from " + source + ")"
	} else {
		source = ""
	}
	fmt.Fprintf(table, "api key\t%s%s\n", view.APIKey, source)
	fmt.Fprintf(table, "base url\t%s\n", view.BaseURL)
	fmt.Fprintf(table, "model\t%s\n", view.Model)
	fmt.Fprintf(table, "resolution\t%s\n", view.Resolution)
	fmt.Fprintf(table, "aspect ratio\t%s\n", view.AspectRatio)
	table.Flush()
	fmt.Fprint(e.stdout, buf.String())
	return nil
}

// configSet writes one key. An empty value removes it.
func (e *env) configSet(key, value string) error {
	file, err := config.Load()
	if err != nil {
		return err
	}
	value = strings.TrimSpace(value)

	switch key {
	case "api-key", "api_key":
		file.APIKey = value
	case "base-url", "base_url":
		if value != "" {
			// 存进去之前先规范化，免得每次调用都要重新猜用户到底填了没填 /v1。
			normalized, err := vergeapi.NormalizeBaseURL(value)
			if err != nil {
				return err
			}
			value = normalized
		}
		file.BaseURL = value
	case "model":
		if value != "" {
			if warning := vergeapi.UnknownModelWarning(value); warning != "" {
				e.warnf("%s", warning)
			}
		}
		file.Model = value
	case "resolution":
		// 分辨率随模型增删，未知值只警告：拦死会让 CLI 在服务端上新档位时立刻过时。
		if value != "" && !knownResolution(value) {
			e.warnf("%q is not a resolution this CLI knows (%s); storing it anyway",
				value, strings.Join(knownResolutions(), ", "))
		}
		file.Resolution = value
	case "aspect-ratio", "aspect_ratio":
		// 宽高比是文档级封闭枚举，写错就是每次生成都失败，这里直接拦。
		if value != "" && !contains(vergeapi.AspectRatios, value) {
			return &vergeapi.ValidationError{
				Field:   "aspect-ratio",
				Message: fmt.Sprintf("%q is not supported; use one of %s", value, strings.Join(vergeapi.AspectRatios, ", ")),
			}
		}
		file.AspectRatio = value
	default:
		return usageErrorf(usageConfig, "unknown config key %q", key)
	}

	path, err := config.Save(file)
	if err != nil {
		return err
	}
	if value == "" {
		e.infof("removed %s from %s", key, path)
		return nil
	}
	shown := value
	if key == "api-key" || key == "api_key" {
		shown = config.MaskKey(value)
	}
	e.infof("set %s = %s in %s", key, shown, path)
	return nil
}

// resolveConfig loads the config file and records the effective settings on e.
func (e *env) resolveConfig() (config.File, error) {
	file, err := config.Load()
	if err != nil {
		return config.File{}, err
	}
	e.cfg = config.Resolve(file, config.Overrides{
		APIKey:  e.global.apiKey,
		BaseURL: e.global.baseURL,
	}, config.Resolved{
		BaseURL:     vergeapi.DefaultBaseURL,
		Model:       vergeapi.DefaultModel,
		Resolution:  vergeapi.DefaultResolution,
		AspectRatio: vergeapi.DefaultAspectRatio,
	})
	return file, nil
}

// knownResolutions is the union of the resolutions of every model this CLI knows.
func knownResolutions() []string {
	seen := map[string]bool{}
	var out []string
	for _, spec := range vergeapi.KnownModels {
		for _, resolution := range spec.Resolutions {
			if !seen[resolution] {
				seen[resolution] = true
				out = append(out, resolution)
			}
		}
	}
	sort.Strings(out)
	return out
}

func knownResolution(value string) bool {
	return contains(knownResolutions(), value)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// errConfigMissing keeps the "no API key" message in one place.
var errConfigMissing = errors.New(
	"no API key found\n" +
		"  set one of:\n" +
		"    export " + config.EnvAPIKey + "=sk-...\n" +
		"    verge config set api-key sk-...\n" +
		"    verge --api-key sk-... <command>",
)
