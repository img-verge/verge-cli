package vergeapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestPrepareRequestWireFormat pins the mixed casing of POST /images/tasks/prepare.
//
// 同一个请求体里 imageCount / fileName / contentType 是 camelCase，而 aspect_ratio、
// image_urls 是 snake_case。这是服务端 DTO 的既定口径，"顺手统一"会让上传静默失败。
func TestPrepareRequestWireFormat(t *testing.T) {
	var captured []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks/prepare", func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"task_id":"task_1","status":"uploading","uploads":[{"id":"up_1","put_url":"https://store.test/put/1","expires_at":"2026-08-10T00:00:00Z"}]}`)
	})
	client := newTestClient(t, mux)

	count, samples := 1, 2
	resp, err := client.Prepare(context.Background(), PrepareRequest{
		Model:       "gpt-image-2",
		Prompt:      "a neon city",
		Resolution:  "2k",
		AspectRatio: "16:9",
		N:           &samples,
		ImageCount:  &count,
		Images:      []PrepareImage{{FileName: "a.png", ContentType: "image/png", Width: 64, Height: 32}},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if resp.TaskID != "task_1" || len(resp.Uploads) != 1 || resp.Uploads[0].PutURL != "https://store.test/put/1" {
		t.Fatalf("unexpected prepare response: %+v", resp)
	}

	body := decodeJSON(t, captured)
	for _, key := range []string{"imageCount", "images", "model", "prompt", "resolution", "aspect_ratio", "n"} {
		if _, ok := body[key]; !ok {
			t.Errorf("prepare body is missing %q: %s", key, captured)
		}
	}
	if _, ok := body["image_count"]; ok {
		t.Errorf("prepare body must use imageCount, not image_count: %s", captured)
	}
	images, ok := body["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("images = %v, want one entry", body["images"])
	}
	image, ok := images[0].(map[string]any)
	if !ok {
		t.Fatalf("images[0] = %v, want an object", images[0])
	}
	for _, key := range []string{"fileName", "contentType", "width", "height"} {
		if _, ok := image[key]; !ok {
			t.Errorf("images[0] is missing %q: %s", key, captured)
		}
	}
	for _, key := range []string{"file_name", "content_type"} {
		if _, ok := image[key]; ok {
			t.Errorf("images[0] must not use snake_case %q: %s", key, captured)
		}
	}
}

// TestOptionalScalarsKeepExplicitZero mirrors the project rule for relay DTOs: an absent
// field disappears from the body, while an explicitly zero one is still sent so the
// server can answer with its own authoritative error.
func TestOptionalScalarsKeepExplicitZero(t *testing.T) {
	var captured []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks", func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"task_id":"task_1","status":"queued"}`)
	})
	client := newTestClient(t, mux)

	if _, err := client.CreateTask(context.Background(), CreateTaskRequest{Model: "m", Prompt: "p"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, ok := decodeJSON(t, captured)["n"]; ok {
		t.Errorf("an unset n must not appear in the body: %s", captured)
	}

	zero := 0
	if _, err := client.CreateTask(context.Background(), CreateTaskRequest{Model: "m", Prompt: "p", N: &zero}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	value, ok := decodeJSON(t, captured)["n"]
	if !ok {
		t.Fatalf("an explicit n=0 must survive marshalling: %s", captured)
	}
	if value != float64(0) {
		t.Errorf("n = %v, want 0", value)
	}
}

func TestReferenceMarshalling(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "unnamed URL collapses to a string", value: ReferenceURL{URL: "https://x.test/a.png"}, want: `"https://x.test/a.png"`},
		{name: "named URL keeps the object form", value: ReferenceURL{URL: "https://x.test/a.png", Name: "logo"}, want: `{"url":"https://x.test/a.png","name":"logo"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.value)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(raw) != test.want {
				t.Errorf("Marshal = %s, want %s", raw, test.want)
			}
		})
	}
}

func TestGetTaskEscapesTaskID(t *testing.T) {
	var path string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		io.WriteString(w, `{"task_id":"weird/id","status":"completed"}`)
	})
	client := newTestClient(t, mux)

	if _, err := client.GetTask(context.Background(), "weird/id"); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if path != "/v1/images/tasks/weird%2Fid" {
		t.Errorf("path = %q, want the id percent-escaped", path)
	}
}

func TestGetTaskSurfacesFailedTaskBody(t *testing.T) {
	// HTTP 200 不代表成功：status=failed 时 data 缺失，原因在 error 里。
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"task_id":"task_1","status":"failed","quota":0,"error":{"message":"内容不合规","code":"content_policy_violation"}}`)
	})
	client := newTestClient(t, mux)

	task, err := client.GetTask(context.Background(), "task_1")
	if err != nil {
		t.Fatalf("GetTask should not turn a failed task into a transport error: %v", err)
	}
	if task.Status != StatusFailed {
		t.Fatalf("Status = %q, want %q", task.Status, StatusFailed)
	}
	if task.Error == nil || task.Error.Code != CodeContentPolicyViolation {
		t.Errorf("Error = %+v, want code %q", task.Error, CodeContentPolicyViolation)
	}
}

func TestIsTerminalStatus(t *testing.T) {
	tests := map[string]bool{
		StatusCompleted:     true,
		StatusFailed:        true,
		StatusUploading:     false,
		StatusQueued:        false,
		StatusSubmitUnknown: false,
		StatusInProgress:    false,
		// 未知状态必须当成非终态，否则服务端新增中间态时客户端会误判成功或失败。
		"materialising": false,
		"":              false,
	}
	for status, want := range tests {
		if got := IsTerminalStatus(status); got != want {
			t.Errorf("IsTerminalStatus(%q) = %t, want %t", status, got, want)
		}
	}
}

func TestModelSupportsImageEndpoint(t *testing.T) {
	if !(Model{SupportedEndpointTypes: []string{"chat", "image-generation"}}).SupportsImageEndpoint() {
		t.Error("a model advertising image-generation should be usable for images")
	}
	if (Model{SupportedEndpointTypes: []string{"chat"}}).SupportsImageEndpoint() {
		t.Error("a chat-only model should not be offered for image tasks")
	}
	if (Model{}).SupportsImageEndpoint() {
		t.Error("a model with no endpoint types should not be assumed to do images")
	}
}
