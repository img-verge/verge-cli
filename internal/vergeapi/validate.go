package vergeapi

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Protocol limits from verge-image-api.md.
const (
	// MaxReferenceImages caps local uploads plus public URLs combined.
	MaxReferenceImages = 7
	// MaxPromptRunes counts Unicode characters, not bytes.
	MaxPromptRunes = 3000
	// MaxSampleCount is the upper bound of n / sample_count.
	MaxSampleCount = 4
	// MaxPublicReferenceBytes is the per-image cap for a public reference URL.
	MaxPublicReferenceBytes = 10 << 20

	DefaultModel       = "gpt-image-2"
	DefaultResolution  = "1080p"
	DefaultAspectRatio = "1:1"
)

// ModelSpec is the locally known capability of one image model.
//
// 这张表只用于「少跑一次往返就能发现的错」，不是权威来源：服务端随时会上新模型，
// 每个 Key 实际可见的列表要看 GET /models。因此未知模型一律放行，只有已知模型才
// 校验分辨率。
type ModelSpec struct {
	ID          string
	DisplayName string
	Resolutions []string
}

// KnownModels mirrors the "模型与分辨率" table in verge-image-api.md.
var KnownModels = []ModelSpec{
	{ID: "gpt-image-2", DisplayName: "GPT Image 2", Resolutions: []string{"1080p", "2k", "4k"}},
	{ID: "gemini-3-pro-image-preview", DisplayName: "Nano Banana Pro", Resolutions: []string{"1080p", "2k", "4k"}},
	{ID: "gemini-3.1-flash-image-preview", DisplayName: "Nano Banana 2", Resolutions: []string{"1080p", "2k", "4k"}},
	{ID: "gemini-3.1-flash-lite-image", DisplayName: "Nano Banana 2 Lite", Resolutions: []string{"1080p"}},
}

// AspectRatios are the ratios every image model accepts.
var AspectRatios = []string{"1:1", "16:9", "9:16", "4:3", "3:4"}

// LookupModel finds a locally known model spec. 模型 ID 大小写必须完全一致，
// 所以这里不做大小写折叠 —— 折叠了反而会把服务端会拒绝的请求放过去。
func LookupModel(id string) (ModelSpec, bool) {
	for _, spec := range KnownModels {
		if spec.ID == id {
			return spec, true
		}
	}
	return ModelSpec{}, false
}

// ValidationError is a client-side parameter rejection, raised before any network call.
type ValidationError struct {
	Field   string
	Message string
	// Hint 给出可执行的下一步，比如换用异步三段式上传。
	Hint string
}

func (e *ValidationError) Error() string {
	head := e.Message
	if e.Field != "" {
		head = e.Field + ": " + e.Message
	}
	if e.Hint != "" {
		return head + "\n  " + e.Hint
	}
	return head
}

// ImageTaskParams is the parameter set shared by image task creation and quota estimation.
type ImageTaskParams struct {
	Model       string
	Prompt      string
	Resolution  string
	AspectRatio string
	N           int
	// ReferenceCount is local uploads plus public URLs.
	ReferenceCount int
}

// Validate checks what can be checked without a round trip.
//
// 刻意不校验模型 ID 本身：新模型上线时硬拦会让 CLI 立刻过时。分辨率只在模型已知时
// 校验，宽高比和张数是稳定的协议级枚举，本地拦掉能省一次往返。
func (p ImageTaskParams) Validate() error {
	if strings.TrimSpace(p.Prompt) == "" {
		return &ValidationError{Field: "prompt", Message: "must not be empty"}
	}
	if runes := utf8.RuneCountInString(p.Prompt); runes > MaxPromptRunes {
		return &ValidationError{
			Field:   "prompt",
			Message: fmt.Sprintf("%d characters exceeds the %d character limit", runes, MaxPromptRunes),
		}
	}
	if p.N < 1 || p.N > MaxSampleCount {
		return &ValidationError{
			Field:   "n",
			Message: fmt.Sprintf("must be between 1 and %d, got %d", MaxSampleCount, p.N),
		}
	}
	if p.AspectRatio != "" && !contains(AspectRatios, p.AspectRatio) {
		return &ValidationError{
			Field:   "aspect-ratio",
			Message: fmt.Sprintf("%q is not supported; use one of %s", p.AspectRatio, strings.Join(AspectRatios, ", ")),
		}
	}
	if spec, known := LookupModel(p.Model); known && p.Resolution != "" && !contains(spec.Resolutions, p.Resolution) {
		return &ValidationError{
			Field: "resolution",
			Message: fmt.Sprintf(
				"%s does not support %q; it supports %s",
				spec.ID, p.Resolution, strings.Join(spec.Resolutions, ", "),
			),
		}
	}
	if p.ReferenceCount > MaxReferenceImages {
		return &ValidationError{
			Field: "reference images",
			Message: fmt.Sprintf(
				"%d reference images exceeds the limit of %d (local uploads and public URLs combined)",
				p.ReferenceCount, MaxReferenceImages,
			),
		}
	}
	return nil
}

// UnknownModelWarning returns a non-fatal note when the model is not in KnownModels.
// 返回空串表示模型已知。
func UnknownModelWarning(model string) string {
	if _, known := LookupModel(model); known {
		return ""
	}
	ids := make([]string, 0, len(KnownModels))
	for _, spec := range KnownModels {
		ids = append(ids, spec.ID)
	}
	sort.Strings(ids)
	return fmt.Sprintf(
		"model %q is not in this CLI's known model list (%s); run `verge models` to see what this key may use",
		model, strings.Join(ids, ", "),
	)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
