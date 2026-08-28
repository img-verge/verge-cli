package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/img-verge/verge-cli/internal/imagefile"
	"github.com/img-verge/verge-cli/internal/vergeapi"
)

// cliResult is what one `Run` invocation produced.
type cliResult struct {
	code   int
	stdout string
	stderr string
}

// newFakeAPI starts a server and points the CLI at it. 同时把 VERGE_CONFIG 指到临时文件、
// 清空两个环境变量：否则测试会读写开发者本机真实配置，结果还依赖本地是否配过 Key。
func newFakeAPI(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("VERGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("VERGE_API_KEY", "")
	t.Setenv("VERGE_API_BASE_URL", "")
	return server
}

// runCLI invokes the CLI the way main does, with credentials passed as flags.
func runCLI(t *testing.T, server *httptest.Server, args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	full := append([]string{"--base-url", server.URL, "--api-key", "sk-test", "--retries", "0"}, args...)
	code := Run(full, &stdout, &stderr)
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// writeNoisePNG writes a random-noise PNG bigger than the 10 MiB upload ceiling:
// deflate can't compress noise, so the file size tracks raw pixels.
func writeNoisePNG(t *testing.T, path string) string {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 2200, 2200))
	rng := rand.New(rand.NewSource(42))
	for y := 0; y < 2200; y++ {
		for x := 0; x < 2200; x++ {
			canvas.SetRGBA(x, y, color.RGBA{
				R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		t.Fatalf("encode noise png: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestRunBalance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want the --api-key value", got)
		}
		io.WriteString(w, `{"object":"credit_summary","wallet_available_quota":1234567,"key_available_quota":null}`)
	})
	server := newFakeAPI(t, mux)

	got := runCLI(t, server, "balance")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	if !strings.Contains(got.stdout, "1,234,567") || !strings.Contains(got.stdout, "unlimited") {
		t.Errorf("stdout = %q, want the wallet quota and an unlimited key", got.stdout)
	}
}

// TestRunJSONIsLosslessPassthrough: --json 的价值就在于服务端加字段时管道下游立刻能看到，
// 所以它必须透传原始响应体，而不是把客户端结构体重新编码一遍。
func TestRunJSONIsLosslessPassthrough(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"credit_summary","wallet_available_quota":900,"key_available_quota":null,"future_field":"keep me"}`)
	})
	server := newFakeAPI(t, mux)

	got := runCLI(t, server, "balance", "--json")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, got.stdout)
	}
	if decoded["future_field"] != "keep me" {
		t.Errorf("stdout = %s, want the unknown field preserved", got.stdout)
	}
}

// TestRunGlobalFlagPositionDoesNotMatter: 全局 flag 在子命令前后都注册了一遍，两种写法
// 必须等价，否则 `verge balance --json` 会安静地回人类可读格式。
func TestRunGlobalFlagPositionDoesNotMatter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"credit_summary","wallet_available_quota":900,"key_available_quota":null}`)
	})
	server := newFakeAPI(t, mux)

	before := runCLI(t, server, "--json", "balance")
	after := runCLI(t, server, "balance", "--json")
	if before.code != ExitOK || after.code != ExitOK {
		t.Fatalf("exits = %d and %d, want both %d", before.code, after.code, ExitOK)
	}
	if before.stdout != after.stdout {
		t.Errorf("`--json balance` printed %q but `balance --json` printed %q", before.stdout, after.stdout)
	}
}

func TestRunHTTPStatusExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode int
	}{
		{name: "401 is an auth problem", status: http.StatusUnauthorized, body: `{"error":{"message":"无效的令牌","code":"invalid_api_key"}}`, wantCode: ExitAuth},
		{name: "403 is an auth problem", status: http.StatusForbidden, body: `{"error":{"message":"denied","code":"permission_denied"}}`, wantCode: ExitAuth},
		{name: "402 is a quota problem", status: http.StatusPaymentRequired, body: `{"error":{"message":"额度不足","code":"insufficient_quota"}}`, wantCode: ExitQuota},
		{name: "429 is rate limiting", status: http.StatusTooManyRequests, body: `{"error":{"message":"slow down","code":"rate_limit_exceeded"}}`, wantCode: ExitRateLimited},
		{name: "500 is a server problem", status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`, wantCode: ExitServer},
		{name: "400 is bad input", status: http.StatusBadRequest, body: `{"error":{"message":"参数不合法","code":"invalid_parameter","param":"aspect_ratio"}}`, wantCode: ExitInvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-Id", "req_42")
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			})
			server := newFakeAPI(t, mux)

			got := runCLI(t, server, "balance")
			if got.code != test.wantCode {
				t.Errorf("exit = %d, want %d\nstderr: %s", got.code, test.wantCode, got.stderr)
			}
			if !strings.HasPrefix(got.stderr, "error: ") {
				t.Errorf("stderr = %q, want it to start with \"error: \"", got.stderr)
			}
			// request_id 是找服务端排查的唯一线索，不能只在 --verbose 下才出现。
			if !strings.Contains(got.stderr, "req_42") {
				t.Errorf("stderr = %q, want the request id", got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want errors kept off stdout", got.stdout)
			}
		})
	}
}

func TestRunUsageErrors(t *testing.T) {
	server := newFakeAPI(t, http.NewServeMux())
	tests := []struct {
		name string
		args []string
	}{
		{name: "no command", args: nil},
		{name: "unknown command", args: []string{"summon"}},
		{name: "unknown help target", args: []string{"help", "summon"}},
		{name: "unknown task subcommand", args: []string{"task", "destroy"}},
		{name: "unknown task help target", args: []string{"help", "task", "destroy"}},
		{name: "task get without an id", args: []string{"task", "get"}},
		{name: "task get with two ids", args: []string{"task", "get", "a", "b"}},
		{name: "task create without a prompt", args: []string{"task", "create"}},
		{name: "balance with a stray argument", args: []string{"balance", "oops"}},
		{name: "config set without a value", args: []string{"config", "set", "model"}},
		{name: "unknown config key", args: []string{"config", "set", "colour", "blue"}},
		{name: "an undefined flag", args: []string{"balance", "--nope"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := runCLI(t, server, test.args...)
			if got.code != ExitUsage {
				t.Errorf("exit = %d, want %d\nstderr: %s", got.code, ExitUsage, got.stderr)
			}
			if got.stderr == "" {
				t.Error("a usage error must explain itself on stderr")
			}
		})
	}
}

// TestRunHelpExitsCleanly: -h 曾经在打印用法之后又补一行 "error: help shown"。
func TestRunHelpExitsCleanly(t *testing.T) {
	server := newFakeAPI(t, http.NewServeMux())
	for _, args := range [][]string{
		{"--help"},
		{"task", "create", "-h"},
		{"task", "create", "--help"},
		{"help", "task"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got := runCLI(t, server, args...)
			if got.code != ExitOK {
				t.Errorf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
			}
			if !strings.Contains(got.stdout, "usage:") {
				t.Errorf("stdout = %q, want the usage text on stdout", got.stdout)
			}
			if strings.Contains(got.stderr, "error") {
				t.Errorf("stderr = %q, want no error line after printing help", got.stderr)
			}
		})
	}
}

// TestRunTaskValidatesBeforeSpendingQuota: 参数写错必须在发请求之前就退出，因为创建任务会
// 预扣额度 —— 让服务端来拒绝等于白花一次调用。
func TestRunTaskValidatesBeforeSpendingQuota(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls++
		t.Errorf("unexpected request to %s %s: local validation should have rejected this", r.Method, r.URL.Path)
	})
	server := newFakeAPI(t, mux)

	tests := []struct {
		name string
		args []string
	}{
		{name: "unsupported aspect ratio", args: []string{"task", "create", "a city", "-a", "21:9"}},
		{name: "too many images", args: []string{"task", "create", "a city", "-n", "9"}},
		{name: "a resolution the model cannot do", args: []string{"task", "create", "a city", "-m", "gemini-3.1-flash-lite-image", "-r", "4k"}},
		{name: "duplicate reference names", args: []string{"task", "create", "[@a] and [@a]", "-u", "a=https://x.test/1.png", "-u", "a=https://x.test/2.png"}},
		{name: "a URL passed to --file", args: []string{"task", "create", "a city", "-f", "https://x.test/1.png"}},
		{name: "a missing reference file", args: []string{"task", "create", "a city", "-f", filepath.Join(t.TempDir(), "nope.png")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := runCLI(t, server, test.args...)
			if got.code != ExitInvalidInput && got.code != ExitError {
				t.Errorf("exit = %d, want a local rejection\nstderr: %s", got.code, got.stderr)
			}
			if got.stderr == "" {
				t.Error("a rejected parameter must say what is wrong")
			}
		})
	}
	if calls != 0 {
		t.Errorf("%d request(s) reached the server, want 0", calls)
	}
}

// TestRunTaskKeepsPromptOutOfFlags is the regression test for argument permutation:
// `verge task create "prompt" -o DIR` must not fold the flag into the prompt.
func TestRunTaskKeepsPromptOutOfFlags(t *testing.T) {
	var sent map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Fatalf("request body is not JSON: %v", err)
		}
		io.WriteString(w, `{"created":1000,"data":[]}`)
	})
	server := newFakeAPI(t, mux)
	dir := filepath.Join(t.TempDir(), "out")

	// 返回空 data 会以错误结束，这里只关心发出去的 prompt。
	runCLI(t, server, "task", "create", "a neon city", "-o", dir, "-r", "2k")
	if sent == nil {
		t.Fatal("no request reached the server")
	}
	if sent["prompt"] != "a neon city" {
		t.Errorf("prompt = %v, want %q with the flags stripped", sent["prompt"], "a neon city")
	}
	if sent["resolution"] != "2k" {
		t.Errorf("resolution = %v, want 2k", sent["resolution"])
	}
}

func TestRunTaskDownloadsResults(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"task_test_dl","task_id":"task_test_dl","status":"completed","data":[{"url":"%s/files/1","revised_prompt":"x"},{"url":"%s/files/2"}]}`, server.URL, server.URL)
	})
	mux.HandleFunc("GET /files/{n}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("a signed result URL points at object storage; the API key must not be sent there")
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes(t))
	})
	server = newFakeAPI(t, mux)
	dir := filepath.Join(t.TempDir(), "out")

	got := runCLI(t, server, "task", "create", "a neon city", "-o", dir)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	for index := 1; index <= 2; index++ {
		// 任务 ID 作为文件名前缀，同一目录下不会覆盖。
		path := filepath.Join(dir, fmt.Sprintf("task_test_dl-%d.png", index))
		stat, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v\nstderr: %s", path, err, got.stderr)
		}
		if stat.Size() == 0 {
			t.Errorf("%s is empty", path)
		}
	}
}

// TestRunDownloadRejectsExpiredSignedURL: 过期的签名链接常常回 HTTP 200 加一段 XML 错误体，
// 只看状态码就落盘会得到一个"下载成功"的垃圾文件。
func TestRunDownloadRejectsExpiredSignedURL(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"task_id":"task_1","status":"completed","quota":4000,"data":[{"url":"%s/files/1"}]}`, server.URL)
	})
	mux.HandleFunc("GET /files/{n}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>`)
	})
	server = newFakeAPI(t, mux)
	dir := filepath.Join(t.TempDir(), "out")

	got := runCLI(t, server, "download", "task_1", "-o", dir)
	if got.code == ExitOK {
		t.Fatalf("exit = %d, want a failure\nstdout: %s", got.code, got.stdout)
	}
	if !strings.Contains(got.stderr, "expired") {
		t.Errorf("stderr = %q, want it to suggest the signed URL expired", got.stderr)
	}
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) != 0 {
		t.Errorf("%d file(s) written, want none: an XML error body must never land on disk", len(entries))
	}
}

func TestRunDownloadRejectsPathPrefix(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"task_id":"task_1","status":"completed","data":[{"url":"https://cdn.test/1.png"}]}`)
	})
	server := newFakeAPI(t, mux)

	got := runCLI(t, server, "download", "task_1", "--prefix", "..\\outside")
	if got.code != ExitError {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitError, got.stderr)
	}
	if !strings.Contains(got.stderr, "must be a file name") {
		t.Errorf("stderr = %q, want a safe-prefix error", got.stderr)
	}
}

func TestRunTaskGetFailedTaskExitsEight(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 且 status=failed：传输成功，任务失败，两件事要用不同出口码。
		io.WriteString(w, `{"task_id":"task_1","status":"failed","quota":0,"error":{"message":"内容不合规","code":"content_policy_violation"}}`)
	})
	server := newFakeAPI(t, mux)

	got := runCLI(t, server, "task", "get", "task_1")
	if got.code != ExitTaskFailed {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitTaskFailed, got.stderr)
	}
	if !strings.Contains(got.stdout, vergeapi.CodeContentPolicyViolation) {
		t.Errorf("stdout = %q, want the stable error code", got.stdout)
	}
}

func TestRunTaskGetWaitTimeoutExitsNine(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"task_id":"task_1","status":"in_progress","quota":0}`)
	})
	server := newFakeAPI(t, mux)

	got := runCLI(t, server, "task", "get", "task_1", "--wait", "--wait-timeout", "150ms", "--poll-interval", "10ms")
	if got.code != ExitWaitTimeout {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitWaitTimeout, got.stderr)
	}
	// 超时不等于失败：文案必须说明任务还在跑，否则用户会以为额度白花了。
	if !strings.Contains(got.stderr, "still running") {
		t.Errorf("stderr = %q, want it to say the task is still running", got.stderr)
	}
}

// TestRunTaskCreateThreeStageUpload walks the whole async path with a local file:
// prepare, a presigned PUT, submit, then polling and download.
func TestRunTaskCreateThreeStageUpload(t *testing.T) {
	var server *httptest.Server
	var (
		prepareBody map[string]any
		submitBody  map[string]any
		putBody     []byte
		putType     string
		putAuth     string
		polls       int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks/prepare", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &prepareBody)
		fmt.Fprintf(w, `{"task_id":"task_1","status":"uploading","uploads":[{"id":"up_1","put_url":"%s/store/up_1","expires_at":"2099-01-01T00:00:00Z"}]}`, server.URL)
	})
	mux.HandleFunc("PUT /store/{id}", func(w http.ResponseWriter, r *http.Request) {
		putBody, _ = io.ReadAll(r.Body)
		putType = r.Header.Get("Content-Type")
		putAuth = r.Header.Get("Authorization")
		w.Header().Set("ETag", `"etag-1"`)
	})
	mux.HandleFunc("POST /v1/images/tasks/submit", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &submitBody)
		io.WriteString(w, `{"task_id":"task_1","status":"queued","quota":0}`)
	})
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		polls++
		if polls < 2 {
			io.WriteString(w, `{"task_id":"task_1","status":"in_progress","quota":0}`)
			return
		}
		fmt.Fprintf(w, `{"task_id":"task_1","status":"completed","quota":4000,"created_at":100,"completed_at":160,"data":[{"url":"%s/files/1"}]}`, server.URL)
	})
	mux.HandleFunc("GET /files/{n}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes(t))
	})
	server = newFakeAPI(t, mux)

	content := pngBytes(t)
	reference := filepath.Join(t.TempDir(), "logo.png")
	if err := os.WriteFile(reference, content, 0o600); err != nil {
		t.Fatalf("write reference: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "out")

	got := runCLI(t, server,
		"task", "create", "put [@logo] on the wall",
		"-f", "logo="+reference,
		"-m", "gpt-image-2", "-r", "2k", "-a", "16:9", "-n", "2", "--group", "engineering",
		"-o", dir,
		"--poll-interval", "10ms", "--wait-timeout", "5s",
	)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}

	// prepare 声明的文件数、名字、类型要与实际 PUT 的一致，签名把 Content-Type 算进去了。
	images, ok := prepareBody["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("prepare images = %v, want one entry", prepareBody["images"])
	}
	image := images[0].(map[string]any)
	if image["fileName"] != "logo.png" || image["contentType"] != "image/png" {
		t.Errorf("prepare image = %v, want logo.png as image/png", image)
	}
	if prepareBody["imageCount"] != float64(1) {
		t.Errorf("imageCount = %v, want 1", prepareBody["imageCount"])
	}
	for field, want := range map[string]any{
		"model":        "gpt-image-2",
		"prompt":       "put [@logo] on the wall",
		"resolution":   "2k",
		"aspect_ratio": "16:9",
		"n":            float64(2),
		"group":        "engineering",
	} {
		if prepareBody[field] != want {
			t.Errorf("prepare %s = %v, want %v", field, prepareBody[field], want)
		}
	}
	if !bytes.Equal(putBody, content) {
		t.Errorf("uploaded %d bytes, want the file's %d", len(putBody), len(content))
	}
	if putType != "image/png" {
		t.Errorf("PUT Content-Type = %q, want image/png", putType)
	}
	if putAuth != "" {
		t.Error("the presigned PUT must not carry our API key")
	}

	// submit 要带上裸 ETag 和名字，否则 [@logo] 在提示词里匹配不到。
	uploads, ok := submitBody["uploads"].([]any)
	if !ok || len(uploads) != 1 {
		t.Fatalf("submit uploads = %v, want one entry", submitBody["uploads"])
	}
	upload := uploads[0].(map[string]any)
	if upload["id"] != "up_1" || upload["etag"] != "etag-1" || upload["name"] != "logo" {
		t.Errorf("submit upload = %v, want id up_1, etag etag-1 (unquoted), name logo", upload)
	}
	if submitBody["task_id"] != "task_1" {
		t.Errorf("submit task_id = %v, want task_1", submitBody["task_id"])
	}
	for field, want := range map[string]any{
		"model":        "gpt-image-2",
		"prompt":       "put [@logo] on the wall",
		"resolution":   "2k",
		"aspect_ratio": "16:9",
		"n":            float64(2),
		"group":        "engineering",
	} {
		if submitBody[field] != want {
			t.Errorf("submit %s = %v, want %v", field, submitBody[field], want)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "task_1-1.png")); err != nil {
		t.Errorf("stat downloaded image: %v\nstderr: %s", err, got.stderr)
	}
}

// TestRunTaskCreateReencodesOversizedReference pins the compression that keeps local
// files under the object-storage PUT ceiling (the server answers 413 beyond 10 MiB):
// prepare must declare image/jpeg and the PUT must carry re-encoded JPEG bytes, not the
// original oversized file.
func TestRunTaskCreateReencodesOversizedReference(t *testing.T) {
	var server *httptest.Server
	var prepareBody, submitBody map[string]any
	var putBody []byte
	var putType string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks/prepare", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &prepareBody)
		fmt.Fprintf(w, `{"task_id":"task_1","status":"uploading","uploads":[{"id":"up_1","put_url":"%s/store/up_1"}]}`, server.URL)
	})
	mux.HandleFunc("PUT /store/{id}", func(w http.ResponseWriter, r *http.Request) {
		putBody, _ = io.ReadAll(r.Body)
		putType = r.Header.Get("Content-Type")
		w.Header().Set("ETag", `"etag-1"`)
	})
	mux.HandleFunc("POST /v1/images/tasks/submit", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &submitBody)
		io.WriteString(w, `{"task_id":"task_1","status":"queued","quota":0}`)
	})
	server = newFakeAPI(t, mux)

	reference := writeNoisePNG(t, filepath.Join(t.TempDir(), "big.png"))
	if info, err := os.Stat(reference); err != nil || info.Size() <= imagefile.MaxUploadBytes {
		t.Fatalf("test reference is %d bytes, want over %d", info.Size(), imagefile.MaxUploadBytes)
	}

	got := runCLI(t, server, "task", "create", "use [@big] please", "-f", "big="+reference, "--wait-timeout", "5s")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	if !strings.Contains(got.stderr, "re-encoded as JPEG") {
		t.Errorf("stderr should warn about the re-encode, got: %s", got.stderr)
	}

	// prepare 必须声明最终 PUT 的 JPEG 类型，而不是原 PNG。
	images, ok := prepareBody["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("prepare images = %v, want one entry", prepareBody["images"])
	}
	image := images[0].(map[string]any)
	if image["contentType"] != "image/jpeg" {
		t.Errorf("prepare contentType = %v, want image/jpeg", image["contentType"])
	}

	// PUT 的是重编码 JPEG 字节（JPEG 魔数 FF D8），且必须 ≤10 MiB。
	if !bytes.HasPrefix(putBody, []byte{0xFF, 0xD8}) {
		t.Errorf("PUT body is not JPEG (missing FF D8 magic), first bytes %x", putBody[:min(4, len(putBody))])
	}
	if putType != "image/jpeg" {
		t.Errorf("PUT Content-Type = %q, want image/jpeg", putType)
	}
	if len(putBody) > imagefile.MaxUploadBytes {
		t.Errorf("PUT body is %d bytes, still over %d", len(putBody), imagefile.MaxUploadBytes)
	}

	uploads, ok := submitBody["uploads"].([]any)
	if !ok || len(uploads) != 1 {
		t.Fatalf("submit uploads = %v, want one entry", submitBody["uploads"])
	}
	upload := uploads[0].(map[string]any)
	if upload["etag"] != "etag-1" || upload["name"] != "big" {
		t.Errorf("submit upload = %v, want etag etag-1 and name big", upload)
	}
}

// TestRunTaskCreateMixedReferencesKeepsLocalAndPublicInputsSeparate verifies that
// prepare receives only local files while submit carries both upload slots and URLs.
func TestRunTaskCreateMixedReferencesKeepsLocalAndPublicInputsSeparate(t *testing.T) {
	var server *httptest.Server
	var prepareBody, submitBody map[string]any
	var putAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks/prepare", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &prepareBody)
		fmt.Fprintf(w, `{"task_id":"task_mixed","status":"uploading","uploads":[{"id":"up_local","put_url":"%s/store/up_local"}]}`, server.URL)
	})
	mux.HandleFunc("PUT /store/{id}", func(w http.ResponseWriter, r *http.Request) {
		putAuth = r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"etag-local"`)
	})
	mux.HandleFunc("POST /v1/images/tasks/submit", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &submitBody)
		io.WriteString(w, `{"task_id":"task_mixed","status":"queued","quota":0}`)
	})
	server = newFakeAPI(t, mux)

	reference := filepath.Join(t.TempDir(), "local.png")
	if err := os.WriteFile(reference, pngBytes(t), 0o600); err != nil {
		t.Fatalf("write reference: %v", err)
	}
	got := runCLI(t, server,
		"task", "create", "blend [@local] with [@remote]",
		"-f", "local="+reference,
		"-u", "remote=https://cdn.test/remote.png",
		"-m", "gpt-image-2", "-r", "2k", "-a", "16:9", "-n", "2", "--group", "mixed",
	)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	images, ok := prepareBody["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("prepare images = %v, want only one local image", prepareBody["images"])
	}
	if _, exists := prepareBody["image_urls"]; exists {
		t.Errorf("prepare unexpectedly contains image_urls: %v", prepareBody["image_urls"])
	}
	if prepareBody["imageCount"] != float64(1) {
		t.Errorf("prepare imageCount = %v, want 1", prepareBody["imageCount"])
	}
	if putAuth != "" {
		t.Errorf("PUT Authorization = %q, want empty", putAuth)
	}

	uploads, ok := submitBody["uploads"].([]any)
	if !ok || len(uploads) != 1 {
		t.Fatalf("submit uploads = %v, want one local upload", submitBody["uploads"])
	}
	upload := uploads[0].(map[string]any)
	if upload["id"] != "up_local" || upload["etag"] != "etag-local" || upload["name"] != "local" {
		t.Errorf("submit upload = %v, want local slot, bare ETag, and name", upload)
	}
	urls, ok := submitBody["image_urls"].([]any)
	if !ok || len(urls) != 1 {
		t.Fatalf("submit image_urls = %v, want one public URL", submitBody["image_urls"])
	}
	remote := urls[0].(map[string]any)
	if remote["url"] != "https://cdn.test/remote.png" || remote["name"] != "remote" {
		t.Errorf("submit public reference = %v, want named remote URL", remote)
	}
}

// TestRunTaskCreateReleasesUploadSessionOnFailure: 上传失败不上报的话任务会一直卡在
// uploading 状态占着预扣的额度，直到会话过期。
func TestRunTaskCreateReleasesUploadSessionOnFailure(t *testing.T) {
	var server *httptest.Server
	var failBody map[string]any
	var failedUpload string
	var submits int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks/prepare", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"task_id":"task_1","status":"uploading","uploads":[{"id":"up_1","put_url":"%s/store/up_1"}]}`, server.URL)
	})
	mux.HandleFunc("PUT /store/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
	})
	mux.HandleFunc("POST /v1/images/tasks/uploads/{upload_id}/fail", func(w http.ResponseWriter, r *http.Request) {
		failedUpload = r.PathValue("upload_id")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &failBody)
		io.WriteString(w, `{"success":true}`)
	})
	mux.HandleFunc("POST /v1/images/tasks/submit", func(w http.ResponseWriter, r *http.Request) {
		submits++
		io.WriteString(w, `{"task_id":"task_1","status":"queued"}`)
	})
	server = newFakeAPI(t, mux)

	reference := filepath.Join(t.TempDir(), "logo.png")
	if err := os.WriteFile(reference, pngBytes(t), 0o600); err != nil {
		t.Fatalf("write reference: %v", err)
	}

	got := runCLI(t, server, "task", "create", "a city", "-f", reference)
	if got.code == ExitOK {
		t.Fatalf("exit = %d, want a failure\nstdout: %s", got.code, got.stdout)
	}
	if failedUpload != "up_1" {
		t.Errorf("released upload = %q, want up_1", failedUpload)
	}
	if failBody["phase"] != "upload" {
		t.Errorf("phase = %v, want upload", failBody["phase"])
	}
	if failBody["http_status"] != float64(http.StatusForbidden) {
		t.Errorf("http_status = %v, want 403", failBody["http_status"])
	}
	if failBody["code"] != "upload_session_forbidden" {
		t.Errorf("code = %v, want upload_session_forbidden", failBody["code"])
	}
	// 传都没传上去就不能 submit，否则服务端会等一个永远不会到的文件。
	if submits != 0 {
		t.Errorf("submit calls = %d, want 0 after an upload failure", submits)
	}
}

// TestRunTaskCreateReturnsCompletedTask 验证直接创建任务的完成态能正常渲染。
func TestRunTaskCreateReturnsCompletedTask(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"task_1","task_id":"task_1","status":"completed","quota":4000,"data":[{"url":"%s/files/1"}]}`, server.URL)
	})
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"task_id":"task_1","status":"completed","quota":4000,"data":[{"url":"%s/files/1"}]}`, server.URL)
	})
	mux.HandleFunc("GET /files/{n}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes(t))
	})
	server = newFakeAPI(t, mux)

	got := runCLI(t, server, "task", "create", "a neon city", "--poll-interval", "10ms", "--wait-timeout", "5s")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	if !strings.Contains(got.stdout, "task_1") {
		t.Errorf("stdout = %q, want the task it fell back to", got.stdout)
	}
}

func TestRunWithoutAPIKeyExplainsHowToSetOne(t *testing.T) {
	server := newFakeAPI(t, http.NewServeMux())
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--base-url", server.URL, "balance"}, &stdout, &stderr)
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	for _, want := range []string{"VERGE_API_KEY", "verge config set api-key", "--api-key"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, missing %q", stderr.String(), want)
		}
	}
}

func TestRunConfigRoundTrip(t *testing.T) {
	server := newFakeAPI(t, http.NewServeMux())

	if got := runCLI(t, server, "config", "set", "model", "gemini-3-pro-image-preview"); got.code != ExitOK {
		t.Fatalf("config set exit = %d\nstderr: %s", got.code, got.stderr)
	}
	// base-url 存进去之前就规范化，免得每次调用都要重新猜有没有 /v1。
	if got := runCLI(t, server, "config", "set", "base-url", "api.verge-ai.xyz"); got.code != ExitOK {
		t.Fatalf("config set base-url exit = %d\nstderr: %s", got.code, got.stderr)
	}
	if got := runCLI(t, server, "config", "set", "api-key", "sk-verge-abcdef123456"); got.code != ExitOK {
		t.Fatalf("config set api-key exit = %d\nstderr: %s", got.code, got.stderr)
	}

	shown := runCLI(t, server, "config", "show", "--json")
	if shown.code != ExitOK {
		t.Fatalf("config show exit = %d\nstderr: %s", shown.code, shown.stderr)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(shown.stdout), &view); err != nil {
		t.Fatalf("config show --json is not JSON: %v\n%s", err, shown.stdout)
	}
	if view["model"] != "gemini-3-pro-image-preview" {
		t.Errorf("model = %v, want the stored value", view["model"])
	}
	// runCLI 传了 --base-url，flag 优先于配置文件。
	if view["base_url"] != server.URL+"/v1" {
		t.Errorf("base_url = %v, want the flag value %q", view["base_url"], server.URL+"/v1")
	}
	// 这个输出经常被贴进 issue，Key 必须是掩码后的。
	key, _ := view["api_key"].(string)
	if strings.Contains(key, "abcdef") || key == "" {
		t.Errorf("api_key = %q, want it masked", key)
	}

	if got := runCLI(t, server, "config", "unset", "model"); got.code != ExitOK {
		t.Fatalf("config unset exit = %d\nstderr: %s", got.code, got.stderr)
	}
	after := runCLI(t, server, "config", "show", "--json")
	if err := json.Unmarshal([]byte(after.stdout), &view); err != nil {
		t.Fatalf("config show --json is not JSON: %v\n%s", err, after.stdout)
	}
	// 清掉之后回到内置默认值，而不是空串。
	if view["model"] != "gpt-image-2" {
		t.Errorf("model = %v, want the built-in default after unset", view["model"])
	}
}

// TestRunConfigRejectsAnUnusableAspectRatio: 宽高比是文档级封闭枚举，存错会让之后每一次
// 生成都失败，所以这里拦死而不是只警告。
func TestRunConfigRejectsAnUnusableAspectRatio(t *testing.T) {
	server := newFakeAPI(t, http.NewServeMux())
	got := runCLI(t, server, "config", "set", "aspect-ratio", "21:9")
	if got.code != ExitInvalidInput {
		t.Errorf("exit = %d, want %d\nstderr: %s", got.code, ExitInvalidInput, got.stderr)
	}
	if !strings.Contains(got.stderr, "16:9") {
		t.Errorf("stderr = %q, want the supported ratios listed", got.stderr)
	}
}

func TestRunModelsFiltersToImageModels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"list","data":[
			{"id":"gpt-image-2","supported_endpoint_types":["image-generation"]},
			{"id":"claude-chat-only","supported_endpoint_types":["chat"]}
		]}`)
	})
	server := newFakeAPI(t, mux)

	got := runCLI(t, server, "models")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	if !strings.Contains(got.stdout, "gpt-image-2") {
		t.Errorf("stdout = %q, want the image model listed", got.stdout)
	}
	if strings.Contains(got.stdout, "claude-chat-only") {
		t.Errorf("stdout = %q, want chat-only models filtered out", got.stdout)
	}
	if all := runCLI(t, server, "models", "--all"); !strings.Contains(all.stdout, "claude-chat-only") {
		t.Errorf("--all stdout = %q, want every visible model", all.stdout)
	}
}

func TestRunQuotaDoesNotCreateATask(t *testing.T) {
	var query string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/image-quota", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		io.WriteString(w, `{"object":"image_quota","model":"gpt-image-2","resolution":"2k","aspect_ratio":"16:9","n":2,"pre_consumed_quota":8000}`)
	})
	mux.HandleFunc("POST /v1/images/tasks", func(w http.ResponseWriter, r *http.Request) {
		t.Error("quota must never create a task")
	})
	server := newFakeAPI(t, mux)

	got := runCLI(t, server, "quota", "-r", "2k", "-a", "16:9", "-n", "2")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	if !strings.Contains(got.stdout, "8,000") {
		t.Errorf("stdout = %q, want the pre-charged quota", got.stdout)
	}
	for _, want := range []string{"resolution=2k", "aspect_ratio=16%3A9", "n=2"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}
}

func TestRunVersion(t *testing.T) {
	server := newFakeAPI(t, http.NewServeMux())
	got := runCLI(t, server, "version")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	if !strings.HasPrefix(got.stdout, "verge-cli ") {
		t.Errorf("stdout = %q, want it to start with \"verge-cli \"", got.stdout)
	}
}

// TestRunPromptFile covers --prompt-file. 位置参数与文件同时给出时，位置参数优先——
// 手打的词是最终意图，静默忽略 --prompt-file，而不是报错。
func TestRunPromptFile(t *testing.T) {
	var sent map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &sent)
		io.WriteString(w, `{"id":"task_prompt","task_id":"task_prompt","status":"completed","data":[{"url":"https://cdn.test/1.png"}]}`)
	})
	server := newFakeAPI(t, mux)

	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	got := runCLI(t, server, "task", "create", "--prompt-file", path)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}
	// 只去掉结尾换行，提示词内部的换行是有意义的。
	if sent["prompt"] != "line one\nline two" {
		t.Errorf("prompt = %q, want the internal newline kept and the trailing one trimmed", sent["prompt"])
	}

	// 位置参数优先：两个都给了就静默忽略 --prompt-file。
	both := runCLI(t, server, "task", "create", "a city", "--prompt-file", path)
	if both.code != ExitOK {
		t.Errorf("exit = %d, want %d when both prompt forms are given\nstderr: %s", both.code, ExitOK, both.stderr)
	}
	if sent["prompt"] != "a city" {
		t.Errorf("prompt = %q, want the positional prompt to win over --prompt-file", sent["prompt"])
	}
}

// TestRunSurfacesRequestIDOnSuccess: 成功输出末尾带上网关的请求 id，方便对账时指认具体
// 那次调用；--json 是无损透传，绝不能把 request_id 混进去。
func TestRunSurfacesRequestIDOnSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Oneapi-Request-Id", "req_success_1")
		io.WriteString(w, `{"object":"credit_summary","wallet_available_quota":1234567,"key_available_quota":null}`)
	})
	server := newFakeAPI(t, mux)

	human := runCLI(t, server, "balance")
	if human.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", human.code, ExitOK, human.stderr)
	}
	if !strings.Contains(human.stdout, "request_id req_success_1") {
		t.Errorf("stdout = %q, want the server request id surfaced", human.stdout)
	}

	jsonOut := runCLI(t, server, "balance", "--json")
	if jsonOut.code != ExitOK {
		t.Fatalf("--json exit = %d, want %d\nstderr: %s", jsonOut.code, ExitOK, jsonOut.stderr)
	}
	if strings.Contains(jsonOut.stdout, "req_success_1") {
		t.Errorf("--json stdout = %q, must stay a lossless passthrough without request_id", jsonOut.stdout)
	}
}

// TestRunErrorRequestIDFromOneAPIHeader: 失败响应的 body 可能没有 request_id（/balance
// 就是如此），网关的 X-Oneapi-Request-Id 头才是可靠来源；客户端从这里取，而不是从 body。
func TestRunErrorRequestIDFromOneAPIHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Oneapi-Request-Id", "req_err_7")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"无效的令牌","code":"invalid_api_key"}}`)
	})
	server := newFakeAPI(t, mux)

	got := runCLI(t, server, "balance")
	if got.code != ExitAuth {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitAuth, got.stderr)
	}
	if !strings.Contains(got.stderr, "req_err_7") {
		t.Errorf("stderr = %q, want the request id taken from the X-Oneapi-Request-Id header", got.stderr)
	}
}

// TestRunPromptFileRejectsOversizeAndInvalidUTF8: 提示词文件超过 1MiB 或夹着非 UTF-8
// 字节时直接拒绝——那几乎必然是传错文件（比如把整张图当提示词），报错比截断诚实。
func TestRunPromptFileRejectsOversizeAndInvalidUTF8(t *testing.T) {
	mux := http.NewServeMux()
	server := newFakeAPI(t, mux)

	dir := t.TempDir()

	oversize := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(oversize, bytes.Repeat([]byte("x"), maxPromptFileBytes+1), 0o600); err != nil {
		t.Fatalf("write oversize prompt: %v", err)
	}
	big := runCLI(t, server, "task", "create", "--prompt-file", oversize)
	if big.code != ExitInvalidInput {
		t.Errorf("oversize exit = %d, want %d\nstderr: %s", big.code, ExitInvalidInput, big.stderr)
	}
	if !strings.Contains(big.stderr, "1MiB") {
		t.Errorf("stderr = %q, want a hint about the 1MiB cap", big.stderr)
	}

	badUTF := filepath.Join(dir, "binary.txt")
	if err := os.WriteFile(badUTF, []byte{0xff, 0xfe, 0x41}, 0o600); err != nil {
		t.Fatalf("write invalid-utf8 prompt: %v", err)
	}
	invalid := runCLI(t, server, "task", "create", "--prompt-file", badUTF)
	if invalid.code != ExitInvalidInput {
		t.Errorf("invalid-utf8 exit = %d, want %d\nstderr: %s", invalid.code, ExitInvalidInput, invalid.stderr)
	}
	if !strings.Contains(invalid.stderr, "UTF-8") {
		t.Errorf("stderr = %q, want a UTF-8 message", invalid.stderr)
	}
}

// TestRunUsageErrorsSuggestTheRightThing: 拼错的命令/子命令/flag 都得到可操作的建议，
// 而不是把整页帮助刷到脸上。
func TestRunUsageErrorsSuggestTheRightThing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"credit_summary","wallet_available_quota":1,"key_available_quota":null}`)
	})
	server := newFakeAPI(t, mux)

	misspelled := runCLI(t, server, "balence")
	if misspelled.code != ExitUsage {
		t.Errorf("balence exit = %d, want %d\nstderr: %s", misspelled.code, ExitUsage, misspelled.stderr)
	}
	if !strings.Contains(misspelled.stderr, `did you mean "balance"`) {
		t.Errorf("stderr = %q, want a did-you-mean hint", misspelled.stderr)
	}

	unknown := runCLI(t, server, "summon")
	if unknown.code != ExitUsage {
		t.Errorf("summon exit = %d, want %d", unknown.code, ExitUsage)
	}
	if !strings.Contains(unknown.stderr, "available:") {
		t.Errorf("stderr = %q, want an available-commands list", unknown.stderr)
	}

	taskSub := runCLI(t, server, "task", "creat")
	if taskSub.code != ExitUsage {
		t.Errorf("task creat exit = %d, want %d", taskSub.code, ExitUsage)
	}
	if !strings.Contains(taskSub.stderr, `did you mean "create"`) {
		t.Errorf("stderr = %q, want a did-you-mean hint for the task subcommand", taskSub.stderr)
	}
	if !strings.Contains(taskSub.stderr, "available: create, get") {
		t.Errorf("stderr = %q, want the task subcommand list", taskSub.stderr)
	}

	badFlag := runCLI(t, server, "balance", "--nope")
	if badFlag.code != ExitUsage {
		t.Errorf("balance --nope exit = %d, want %d\nstderr: %s", badFlag.code, ExitUsage, badFlag.stderr)
	}
	if !strings.Contains(badFlag.stderr, "tip: run `verge-cli balance --help` for the available flags") {
		t.Errorf("stderr = %q, want a flag help tip", badFlag.stderr)
	}
}

// TestRunErrorMessagesCarryTheStep: 多步流程出错时，错误消息带上是哪一步失败的，脚本
// 才知道该看哪个环节。%w 包裹不改变出口码分类。
func TestRunErrorMessagesCarryTheStep(t *testing.T) {
	var prepareCalled bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad params","code":"invalid_parameter","param":"aspect_ratio"}}`)
	})
	mux.HandleFunc("POST /v1/images/tasks/prepare", func(w http.ResponseWriter, r *http.Request) {
		prepareCalled = true
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad prepare","code":"invalid_parameter"}}`)
	})
	server := newFakeAPI(t, mux)

	createResult := runCLI(t, server, "task", "create", "a city")
	if createResult.code != ExitInvalidInput {
		t.Errorf("task create exit = %d, want %d\nstderr: %s", createResult.code, ExitInvalidInput, createResult.stderr)
	}
	if !strings.Contains(createResult.stderr, "create: ") {
		t.Errorf("stderr = %q, want the create step named", createResult.stderr)
	}

	// 三段式上传的 prepare 阶段失败，错误消息要能定位到 prepare。
	content := pngBytes(t)
	reference := filepath.Join(t.TempDir(), "logo.png")
	if err := os.WriteFile(reference, content, 0o600); err != nil {
		t.Fatalf("write reference: %v", err)
	}
	upload := runCLI(t, server, "task", "create", "a city", "-f", "logo="+reference)
	if !prepareCalled {
		t.Fatal("expected prepare to have been called")
	}
	if upload.code != ExitInvalidInput {
		t.Errorf("task create exit = %d, want %d\nstderr: %s", upload.code, ExitInvalidInput, upload.stderr)
	}
	if !strings.Contains(upload.stderr, "prepare: ") {
		t.Errorf("stderr = %q, want the prepare step named", upload.stderr)
	}
}

// TestRunHelpShowsTaskConstraints: 所有能接生成参数的 help 路径（help task create、
// task create --help、help task create、task create --help）都内嵌一份客户端侧约束表。
func TestRunHelpShowsTaskConstraints(t *testing.T) {
	server := newFakeAPI(t, http.NewServeMux()) // help 不发请求，server 只需存在
	checks := []string{"gpt-image-2", "1080p, 2k, 4k", "1:1", "1-4", "at most 7", "3000"}
	for _, args := range [][]string{
		{"task", "create", "--help"},
		{"help", "task", "create"},
	} {
		got := runCLI(t, server, args...)
		if got.code != ExitOK {
			t.Errorf("%v exit = %d, want %d\nstderr: %s", args, got.code, ExitOK, got.stderr)
			continue
		}
		for _, want := range checks {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("%v stdout missing %q", args, want)
			}
		}
	}
}

func TestRunTaskCreateMixedNamedBase64References(t *testing.T) {
	mux := http.NewServeMux()
	var server *httptest.Server
	var prepareBody, submitBody map[string]any
	putBodies := map[string][]byte{}
	putAuth := map[string]string{}
	putLengths := map[string]int64{}

	mux.HandleFunc("POST /v1/images/tasks/prepare", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &prepareBody)
		fmt.Fprintf(w, `{"task_id":"task_b64","status":"uploading","uploads":[`+
			`{"id":"up_local","put_url":"%s/store/up_local"},`+
			`{"id":"up_file","put_url":"%s/store/up_file"},`+
			`{"id":"up_inline","put_url":"%s/store/up_inline"}]}`, server.URL, server.URL, server.URL)
	})
	mux.HandleFunc("PUT /store/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		putBodies[id], _ = io.ReadAll(r.Body)
		putAuth[id] = r.Header.Get("Authorization")
		putLengths[id] = r.ContentLength
		w.Header().Set("ETag", `"etag-`+id+`"`)
	})
	mux.HandleFunc("POST /v1/images/tasks/submit", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &submitBody)
		io.WriteString(w, `{"task_id":"task_b64","status":"queued","quota":0}`)
	})
	server = newFakeAPI(t, mux)

	content := pngBytes(t)
	local := filepath.Join(t.TempDir(), "local.png")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatalf("write local: %v", err)
	}
	encodedFile := filepath.Join(t.TempDir(), "encoded.txt")
	encoded := base64.StdEncoding.EncodeToString(content)
	if err := os.WriteFile(encodedFile, []byte(encoded), 0o600); err != nil {
		t.Fatalf("write encoded: %v", err)
	}
	inline := "data:image/png;base64," + encoded

	got := runCLI(t, server,
		"task", "create", "blend [@local], [@encoded], [@inline] and [@public]",
		"-f", "local="+local,
		"--base64-file", "encoded="+encodedFile,
		"--base64-data", "inline="+inline,
		"-u", "public=https://images.test/public.png",
		"-m", "gpt-image-2", "-r", "2k", "-a", "16:9", "-n", "2", "--group", "engineering",
	)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitOK, got.stderr)
	}

	images, ok := prepareBody["images"].([]any)
	if !ok || len(images) != 3 || prepareBody["imageCount"] != float64(3) {
		t.Fatalf("prepare images/imageCount = %v/%v, want 3/3", prepareBody["images"], prepareBody["imageCount"])
	}
	if _, exists := prepareBody["image_urls"]; exists {
		t.Error("public URLs must not be sent to prepare")
	}
	for field, want := range map[string]any{"model": "gpt-image-2", "resolution": "2k", "aspect_ratio": "16:9", "n": float64(2), "group": "engineering"} {
		if prepareBody[field] != want || submitBody[field] != want {
			t.Errorf("%s prepare/submit = %v/%v, want %v", field, prepareBody[field], submitBody[field], want)
		}
	}
	for _, id := range []string{"up_local", "up_file", "up_inline"} {
		if !bytes.Equal(putBodies[id], content) {
			t.Errorf("PUT %s body differs from decoded image", id)
		}
		if putAuth[id] != "" {
			t.Errorf("PUT %s Authorization = %q, want empty", id, putAuth[id])
		}
		if putLengths[id] != int64(len(content)) {
			t.Errorf("PUT %s Content-Length = %d, want %d", id, putLengths[id], len(content))
		}
	}

	uploads, ok := submitBody["uploads"].([]any)
	if !ok || len(uploads) != 3 {
		t.Fatalf("submit uploads = %v, want three", submitBody["uploads"])
	}
	wantNames := []string{"local", "encoded", "inline"}
	for i, raw := range uploads {
		upload := raw.(map[string]any)
		if upload["name"] != wantNames[i] {
			t.Errorf("uploads[%d].name = %v, want %q", i, upload["name"], wantNames[i])
		}
		wantETag := "etag-up_" + map[int]string{0: "local", 1: "file", 2: "inline"}[i]
		if upload["etag"] != wantETag {
			t.Errorf("uploads[%d].etag = %v, want %q", i, upload["etag"], wantETag)
		}
	}
	urls, ok := submitBody["image_urls"].([]any)
	if !ok || len(urls) != 1 || urls[0].(map[string]any)["name"] != "public" {
		t.Errorf("submit image_urls = %v, want named public URL", submitBody["image_urls"])
	}
}

func TestRunTaskCreateSubmitFailureIncludesPreparedTaskIDAndDoesNotRetry(t *testing.T) {
	mux := http.NewServeMux()
	var server *httptest.Server
	submitCalls := 0
	mux.HandleFunc("POST /v1/images/tasks/prepare", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"task_id":"task_resume","status":"uploading","uploads":[{"id":"up_1","put_url":"%s/store/up_1"}]}`, server.URL)
	})
	mux.HandleFunc("PUT /store/up_1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"etag-1"`)
	})
	mux.HandleFunc("POST /v1/images/tasks/submit", func(w http.ResponseWriter, r *http.Request) {
		submitCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"temporarily unavailable","code":"generation_unavailable"}}`)
	})
	server = newFakeAPI(t, mux)
	path := filepath.Join(t.TempDir(), "ref.png")
	if err := os.WriteFile(path, pngBytes(t), 0o600); err != nil {
		t.Fatalf("write ref: %v", err)
	}

	got := runCLI(t, server, "task", "create", "use [@ref]", "-f", "ref="+path, "--retries", "3")
	if got.code != ExitServer {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitServer, got.stderr)
	}
	if submitCalls != 1 {
		t.Errorf("submit calls = %d, want one non-retried POST", submitCalls)
	}
	for _, want := range []string{"task_resume", "verge-cli task get task_resume", "do not repeat submit"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr = %q, want %q", got.stderr, want)
		}
	}
}

func TestRunTaskCreateSlotMismatchReportsTotalAndTaskID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks/prepare", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"task_id":"task_slots","status":"uploading","uploads":[{"id":"only_one","put_url":"https://store.test/one"}]}`)
	})
	server := newFakeAPI(t, mux)
	encoded := base64.StdEncoding.EncodeToString(pngBytes(t))
	got := runCLI(t, server, "task", "create", "two refs", "--base64-data", "a="+encoded, "--base64-data", "b="+encoded)
	if got.code != ExitError {
		t.Fatalf("exit = %d, want %d\nstderr: %s", got.code, ExitError, got.stderr)
	}
	for _, want := range []string{"task_slots", "for 2 file(s)"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr = %q, want %q", got.stderr, want)
		}
	}
}
