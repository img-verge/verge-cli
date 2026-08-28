package app

import (
	"strings"
	"testing"

	"github.com/img-verge/verge-cli/internal/vergeapi"
)

func TestFormatQuota(t *testing.T) {
	tests := map[int]string{
		0:        "0",
		7:        "7",
		999:      "999",
		1000:     "1,000",
		4000:     "4,000",
		12345:    "12,345",
		1234567:  "1,234,567",
		-1234:    "-1,234",
		-999:     "-999",
		100000:   "100,000",
		1000000:  "1,000,000",
		-1000000: "-1,000,000",
	}
	for value, want := range tests {
		if got := formatQuota(value); got != want {
			t.Errorf("formatQuota(%d) = %q, want %q", value, got, want)
		}
	}
}

// TestRenderBalanceSeparatesUnlimitedFromZero: 一个"无上限"的 Key 和一个"上限已用尽"的
// Key 都会让 key_available_quota 看起来无害，但含义相反，渲染必须能区分。
func TestRenderBalanceSeparatesUnlimitedFromZero(t *testing.T) {
	unlimited := renderBalance(&vergeapi.Balance{WalletAvailableQuota: 1234567})
	if !strings.Contains(unlimited, "unlimited") {
		t.Errorf("render = %q, want it to say unlimited when there is no per-key cap", unlimited)
	}
	if !strings.Contains(unlimited, "1,234,567") {
		t.Errorf("render = %q, want the wallet quota with separators", unlimited)
	}

	zero := 0
	exhausted := renderBalance(&vergeapi.Balance{WalletAvailableQuota: 500, KeyAvailableQuota: &zero})
	if strings.Contains(exhausted, "unlimited") {
		t.Errorf("render = %q, want an exhausted key cap to print as 0", exhausted)
	}
	if !strings.Contains(exhausted, "key available") {
		t.Errorf("render = %q, want a key available row", exhausted)
	}
}

// TestRenderTaskDistinguishesUnsettledQuota: 排队中的任务 quota 是 0，把它渲染成
// "0" 会被读成"免费"，而实际只是还没结算。
func TestRenderTaskDistinguishesUnsettledQuota(t *testing.T) {
	queued := renderTask(&vergeapi.Task{TaskID: "task_1", Status: vergeapi.StatusQueued})
	if !strings.Contains(queued, "not settled yet") {
		t.Errorf("render = %q, want the quota row to say it is not settled", queued)
	}

	done := renderTask(&vergeapi.Task{
		TaskID: "task_1",
		Status: vergeapi.StatusCompleted,
		Quota:  4000,
		Data:   []vergeapi.ImageData{{URL: "https://cdn.test/1.png"}},
	})
	if !strings.Contains(done, "4,000") {
		t.Errorf("render = %q, want the settled quota", done)
	}
	if strings.Contains(done, "not settled") {
		t.Errorf("render = %q, want a settled quota for a completed task", done)
	}
}

// TestRenderTaskLeadsWithTheStableErrorCode keeps the machine-readable half of a failure
// visible: callers should branch on code, not parse the message.
func TestRenderTaskLeadsWithTheStableErrorCode(t *testing.T) {
	got := renderTask(&vergeapi.Task{
		TaskID: "task_1",
		Status: vergeapi.StatusFailed,
		Error: &vergeapi.TaskError{
			Message: "内容不合规",
			Code:    vergeapi.CodeContentPolicyViolation,
			Param:   "prompt",
		},
	})
	for _, want := range []string{"failed", vergeapi.CodeContentPolicyViolation, "prompt", "内容不合规"} {
		if !strings.Contains(got, want) {
			t.Errorf("render = %q, missing %q", got, want)
		}
	}
}

func TestRenderTaskHandlesNil(t *testing.T) {
	if got := renderTask(nil); got != "" {
		t.Errorf("renderTask(nil) = %q, want empty", got)
	}
}

// TestRenderImagesWarnsAboutExpiry: 链接 7 天后失效，不说这件事用户会把 URL 存进文档，
// 一周后再回来发现全是死链。
func TestRenderImagesWarnsAboutExpiry(t *testing.T) {
	got := renderImages([]vergeapi.ImageData{
		{URL: "https://cdn.test/1.png", RevisedPrompt: "a neon city, rain"},
		{URL: "https://cdn.test/2.png"},
	})
	for _, want := range []string{"2 image(s)", "https://cdn.test/1.png", "https://cdn.test/2.png", "a neon city, rain", "expire", "-o"} {
		if !strings.Contains(got, want) {
			t.Errorf("render = %q, missing %q", got, want)
		}
	}
	if renderImages(nil) != "" {
		t.Error("renderImages(nil) should render nothing")
	}
}

// TestRenderModelsLabelsItsOwnGuesswork: RESOLUTIONS 来自本地表而不是接口，不标明来源
// 会让用户把它当成服务端权威口径。
func TestRenderModelsLabelsItsOwnGuesswork(t *testing.T) {
	got := renderModels(&vergeapi.ModelList{Data: []vergeapi.Model{
		{ID: "zzz-model", SupportedEndpointTypes: []string{"image-generation"}},
		{ID: "gpt-image-2", SupportedEndpointTypes: []string{"chat", "image-generation"}},
	}})
	// 排序稳定，输出可 diff。
	if strings.Index(got, "gpt-image-2") > strings.Index(got, "zzz-model") {
		t.Errorf("render = %q, want models sorted by id", got)
	}
	if !strings.Contains(got, "unknown to this CLI") {
		t.Errorf("render = %q, want an unknown model's resolutions marked as unknown", got)
	}
	if !strings.Contains(got, "not the API") {
		t.Errorf("render = %q, want a note that RESOLUTIONS is local knowledge", got)
	}
	if !strings.Contains(got, "1080p") {
		t.Errorf("render = %q, want the known model's resolutions", got)
	}

	empty := renderModels(&vergeapi.ModelList{})
	if !strings.Contains(empty, "no models available") {
		t.Errorf("render = %q, want an explicit empty-list message", empty)
	}
	if renderModels(nil) == "" {
		t.Error("renderModels(nil) should still say something rather than print nothing")
	}
}
