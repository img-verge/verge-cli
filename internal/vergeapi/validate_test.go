package vergeapi

import (
	"errors"
	"strings"
	"testing"
)

func TestImageTaskParamsValidate(t *testing.T) {
	tests := []struct {
		name      string
		params    ImageTaskParams
		wantField string
	}{
		{
			name:   "a fully specified request passes",
			params: ImageTaskParams{Model: "gpt-image-2", Prompt: "a neon city", Resolution: "4k", AspectRatio: "16:9", N: 4, ReferenceCount: 7},
		},
		{
			name:   "an unknown model is allowed through",
			params: ImageTaskParams{Model: "some-model-shipped-tomorrow", Prompt: "p", Resolution: "8k", N: 1},
		},
		{
			name:   "an empty resolution defers to the server default",
			params: ImageTaskParams{Model: "gpt-image-2", Prompt: "p", N: 1},
		},
		{name: "an empty prompt is rejected", params: ImageTaskParams{Model: "gpt-image-2", Prompt: "", N: 1}, wantField: "prompt"},
		{name: "a whitespace-only prompt is rejected", params: ImageTaskParams{Model: "gpt-image-2", Prompt: " \n\t ", N: 1}, wantField: "prompt"},
		{
			name:      "an over-long prompt is rejected",
			params:    ImageTaskParams{Model: "gpt-image-2", Prompt: strings.Repeat("霓", MaxPromptRunes+1), N: 1},
			wantField: "prompt",
		},
		{name: "n below one is rejected", params: ImageTaskParams{Model: "gpt-image-2", Prompt: "p", N: 0}, wantField: "n"},
		{name: "n above the cap is rejected", params: ImageTaskParams{Model: "gpt-image-2", Prompt: "p", N: MaxSampleCount + 1}, wantField: "n"},
		{
			name:      "an unsupported aspect ratio is rejected",
			params:    ImageTaskParams{Model: "gpt-image-2", Prompt: "p", AspectRatio: "21:9", N: 1},
			wantField: "aspect-ratio",
		},
		{
			name:      "a known model rejects a resolution it cannot do",
			params:    ImageTaskParams{Model: "gemini-3.1-flash-lite-image", Prompt: "p", Resolution: "4k", N: 1},
			wantField: "resolution",
		},
		{
			name:      "too many reference images is rejected",
			params:    ImageTaskParams{Model: "gpt-image-2", Prompt: "p", N: 1, ReferenceCount: MaxReferenceImages + 1},
			wantField: "reference images",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.params.Validate()
			if test.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Validate() = %v (%T), want *ValidationError", err, err)
			}
			if validationErr.Field != test.wantField {
				t.Errorf("Field = %q, want %q", validationErr.Field, test.wantField)
			}
			// 报错必须说明"该怎么改"，只说"非法"用户无从下手。
			if strings.TrimSpace(validationErr.Message) == "" {
				t.Error("Message must explain what is wrong")
			}
		})
	}
}

// TestValidationErrorHintIsRendered keeps the actionable recovery hint visible.
func TestValidationErrorHintIsRendered(t *testing.T) {
	err := &ValidationError{Field: "reference images", Message: "too large", Hint: "run `verge task create` instead"}
	got := err.Error()
	for _, want := range []string{"reference images", "too large", "verge task create"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, missing %q", got, want)
		}
	}

	bare := &ValidationError{Message: "must not be empty"}
	if bare.Error() != "must not be empty" {
		t.Errorf("Error() = %q, want no field prefix and no hint line", bare.Error())
	}
}

// TestLookupModelIsCaseSensitive: 模型 ID 会原样发给服务端，大小写折叠只会把注定被拒的
// 请求放过去，还顺带跳过分辨率校验。
func TestLookupModelIsCaseSensitive(t *testing.T) {
	spec, known := LookupModel("gpt-image-2")
	if !known {
		t.Fatal("gpt-image-2 should be a known model")
	}
	if len(spec.Resolutions) == 0 || spec.DisplayName == "" {
		t.Errorf("spec = %+v, want resolutions and a display name", spec)
	}
	if _, known := LookupModel("GPT-Image-2"); known {
		t.Error("LookupModel must not fold case")
	}
	if _, known := LookupModel(""); known {
		t.Error("an empty model id must not resolve to a spec")
	}
}

func TestUnknownModelWarning(t *testing.T) {
	if got := UnknownModelWarning("gpt-image-2"); got != "" {
		t.Errorf("UnknownModelWarning for a known model = %q, want empty", got)
	}
	got := UnknownModelWarning("dall-e-9")
	if got == "" {
		t.Fatal("an unknown model should produce a warning")
	}
	// 警告要同时给出"我不认识它"和"去哪查真正可用的列表"。
	for _, want := range []string{"dall-e-9", "gpt-image-2", "verge models"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q is missing %q", got, want)
		}
	}
}

// TestKnownModelsTableIsUsable guards the one property the rest of the code assumes:
// every known model can validate a resolution, and the defaults are self-consistent.
func TestKnownModelsTableIsUsable(t *testing.T) {
	for _, spec := range KnownModels {
		if spec.ID == "" || len(spec.Resolutions) == 0 {
			t.Errorf("model %+v needs an id and at least one resolution", spec)
		}
	}
	if err := (ImageTaskParams{
		Model:       DefaultModel,
		Prompt:      "p",
		Resolution:  DefaultResolution,
		AspectRatio: DefaultAspectRatio,
		N:           1,
	}).Validate(); err != nil {
		t.Errorf("the built-in defaults must validate against each other: %v", err)
	}
}
