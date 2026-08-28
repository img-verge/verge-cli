package vergeapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points a Client at a local server. BaseURL 故意不带 /v1，顺带验证
// NormalizeBaseURL 会补上，且所有 handler 路径都在 /v1 之下。
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Options{BaseURL: server.URL, APIKey: "sk-test", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty falls back to the documented endpoint", in: "", want: DefaultBaseURL},
		{name: "bare host gains https and /v1", in: "api.verge-ai.xyz", want: "https://api.verge-ai.xyz/v1"},
		{name: "trailing slash is trimmed before /v1 is added", in: "https://api.verge-ai.xyz/", want: "https://api.verge-ai.xyz/v1"},
		{name: "an existing /v1 is left alone", in: "https://api.verge-ai.xyz/v1", want: "https://api.verge-ai.xyz/v1"},
		{name: "self hosted prefix keeps its own path", in: "https://gw.example.com/gateway/v1", want: "https://gw.example.com/gateway/v1"},
		{name: "http survives for local gateways", in: "http://127.0.0.1:3000", want: "http://127.0.0.1:3000/v1"},
		{name: "query and fragment are dropped", in: "https://api.verge-ai.xyz/v1?token=x#y", want: "https://api.verge-ai.xyz/v1"},
		{name: "whitespace is trimmed", in: "  https://api.verge-ai.xyz  ", want: "https://api.verge-ai.xyz/v1"},
		{name: "non http scheme is rejected", in: "ftp://api.verge-ai.xyz", wantErr: true},
		{name: "missing host is rejected", in: "https://", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeBaseURL(test.in)
			if test.wantErr {
				if err == nil {
					t.Fatalf("NormalizeBaseURL(%q) = %q, want an error", test.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeBaseURL(%q): %v", test.in, err)
			}
			if got != test.want {
				t.Errorf("NormalizeBaseURL(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(Options{BaseURL: DefaultBaseURL}); err == nil {
		t.Fatal("New without an API key should fail instead of sending unauthenticated requests")
	}
}

// TestRetryOnlyIdempotentRequests locks the billing-safety rule: creating a task
// pre-charges quota, so a POST must reach the server at most once even when the failure
// looks transient.
func TestRetryOnlyIdempotentRequests(t *testing.T) {
	var gets, posts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
		if gets.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"busy","code":"generation_unavailable"}}`)
			return
		}
		io.WriteString(w, `{"object":"credit_summary","wallet_available_quota":900,"key_available_quota":null}`)
	})
	mux.HandleFunc("POST /v1/images/tasks", func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"busy","code":"generation_unavailable"},"request_id":"req_9"}`)
	})

	client := newTestClient(t, mux)
	client.MaxRetries = 2

	balance, err := client.Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if gets.Load() != 2 {
		t.Errorf("GET attempts = %d, want 2 (one failure plus one retry)", gets.Load())
	}
	if !balance.Unlimited() {
		t.Error("key_available_quota null means an uncapped key, not zero remaining")
	}
	if balance.WalletAvailableQuota != 900 {
		t.Errorf("WalletAvailableQuota = %d, want 900", balance.WalletAvailableQuota)
	}

	_, err = client.CreateTask(context.Background(), CreateTaskRequest{Model: "gpt-image-2", Prompt: "p"})
	if err == nil {
		t.Fatal("CreateTask should surface the 503 instead of hiding it behind retries")
	}
	if posts.Load() != 1 {
		t.Errorf("POST attempts = %d, want 1: task creation pre-charges quota and must never be retried", posts.Load())
	}
	apiErr := AsAPIError(err)
	if apiErr == nil {
		t.Fatalf("CreateTask error = %T, want *APIError", err)
	}
	if apiErr.RequestID != "req_9" {
		t.Errorf("RequestID = %q, want req_9: it is the only usable handle for support", apiErr.RequestID)
	}
	if !IsCode(err, CodeImageServiceUnavailable) {
		t.Errorf("Code = %q, want %q", apiErr.Code, CodeImageServiceUnavailable)
	}
}

func TestRequestHeaders(t *testing.T) {
	var auth, accept, userAgent, contentType string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		auth, accept, userAgent = r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("User-Agent")
		io.WriteString(w, `{"object":"list","data":[]}`)
	})
	mux.HandleFunc("POST /v1/images/tasks", func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		io.WriteString(w, `{"task_id":"task_1","status":"queued"}`)
	})

	client := newTestClient(t, mux)
	client.UserAgent = "verge-cli/test"
	if _, err := client.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if _, err := client.CreateTask(context.Background(), CreateTaskRequest{Model: "m", Prompt: "p"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer sk-test")
	}
	if accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", accept)
	}
	if userAgent != "verge-cli/test" {
		t.Errorf("User-Agent = %q, want verge-cli/test", userAgent)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type on POST = %q, want application/json", contentType)
	}
}

func TestImageQuotaQueryParameters(t *testing.T) {
	var query string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/image-quota", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		io.WriteString(w, `{"object":"image_quota","model":"gpt-image-2","resolution":"2k","aspect_ratio":"16:9","n":2,"pre_consumed_quota":4000}`)
	})
	client := newTestClient(t, mux)

	quota, err := client.ImageQuota(context.Background(), ImageQuotaParams{
		Model: "gpt-image-2", Resolution: "2k", AspectRatio: "16:9", N: 2,
	})
	if err != nil {
		t.Fatalf("ImageQuota: %v", err)
	}
	// aspect_ratio 是 snake_case，写成 aspectRatio 服务端会当成没传。
	for _, want := range []string{"model=gpt-image-2", "resolution=2k", "aspect_ratio=16%3A9", "n=2"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}
	if quota.PreConsumedQuota != 4000 {
		t.Errorf("PreConsumedQuota = %d, want 4000", quota.PreConsumedQuota)
	}
}

func TestImageQuotaOmitsUnsetSampleCount(t *testing.T) {
	var query string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/image-quota", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		io.WriteString(w, `{"object":"image_quota","n":1}`)
	})
	client := newTestClient(t, mux)
	if _, err := client.ImageQuota(context.Background(), ImageQuotaParams{Model: "gpt-image-2"}); err != nil {
		t.Fatalf("ImageQuota: %v", err)
	}
	if strings.Contains(query, "n=") {
		t.Errorf("query %q should omit n so the server applies its own default", query)
	}
}

func TestDecodeAPIError(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantCode      string
		wantMessage   string
		wantRequestID string
		wantString    string
	}{
		{
			name:          "full envelope",
			status:        http.StatusBadRequest,
			body:          `{"error":{"message":"参数不合法","type":"invalid_request_error","param":"aspect_ratio","code":"invalid_parameter"},"request_id":"req_1"}`,
			wantCode:      CodeInvalidParameter,
			wantMessage:   "参数不合法",
			wantRequestID: "req_1",
			wantString:    "参数不合法 (invalid_parameter: aspect_ratio) [request_id req_1]",
		},
		{
			name:        "code without param",
			status:      http.StatusPaymentRequired,
			body:        `{"error":{"message":"额度不足","code":"insufficient_quota"}}`,
			wantCode:    CodeInsufficientQuota,
			wantMessage: "额度不足",
			wantString:  "额度不足 (insufficient_quota)",
		},
		{
			name:        "gateway HTML keeps the status meaningful",
			status:      http.StatusBadGateway,
			body:        "<html><body>502 Bad Gateway</body></html>",
			wantMessage: "Bad Gateway",
			wantString:  "Bad Gateway",
		},
		{
			name:        "empty body falls back to the status text",
			status:      http.StatusTooManyRequests,
			body:        "",
			wantMessage: "Too Many Requests",
			wantString:  "Too Many Requests",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			apiErr := decodeAPIError(test.status, []byte(test.body))
			if apiErr.Status != test.status {
				t.Errorf("Status = %d, want %d", apiErr.Status, test.status)
			}
			if apiErr.Code != test.wantCode {
				t.Errorf("Code = %q, want %q", apiErr.Code, test.wantCode)
			}
			if apiErr.Message != test.wantMessage {
				t.Errorf("Message = %q, want %q", apiErr.Message, test.wantMessage)
			}
			if apiErr.RequestID != test.wantRequestID {
				t.Errorf("RequestID = %q, want %q", apiErr.RequestID, test.wantRequestID)
			}
			if got := apiErr.Error(); got != test.wantString {
				t.Errorf("Error() = %q, want %q", got, test.wantString)
			}
			// 原始体永远留着：解析失败时它是唯一的线索。
			if apiErr.Body != test.body {
				t.Errorf("Body = %q, want %q", apiErr.Body, test.body)
			}
		})
	}
}

func TestAPIErrorRequestIDFallsBackToHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req_header")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"message":"denied","code":"permission_denied"}}`)
	})
	client := newTestClient(t, mux)

	_, err := client.Models(context.Background())
	apiErr := AsAPIError(err)
	if apiErr == nil {
		t.Fatalf("Models error = %T, want *APIError", err)
	}
	if apiErr.RequestID != "req_header" {
		t.Errorf("RequestID = %q, want req_header", apiErr.RequestID)
	}
}

func TestIsCodeIgnoresUnrelatedErrors(t *testing.T) {
	if IsCode(io.EOF, CodeInvalidRequest) {
		t.Error("IsCode must not claim a transport error carries an API code")
	}
	if AsAPIError(io.EOF) != nil {
		t.Error("AsAPIError should return nil for non-API errors")
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{name: "delta seconds", in: "5", want: 5 * time.Second},
		{name: "zero means no hint", in: "0", want: 0},
		{name: "negative is ignored", in: "-3", want: 0},
		{name: "empty is ignored", in: "", want: 0},
		{name: "garbage is ignored", in: "soon", want: 0},
		{name: "past HTTP date is ignored", in: "Mon, 02 Jan 2006 15:04:05 GMT", want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseRetryAfter(test.in); got != test.want {
				t.Errorf("parseRetryAfter(%q) = %s, want %s", test.in, got, test.want)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	if got := retryDelay(1, nil); got != 500*time.Millisecond {
		t.Errorf("retryDelay(1, nil) = %s, want 500ms", got)
	}
	if got := retryDelay(2, nil); got != time.Second {
		t.Errorf("retryDelay(2, nil) = %s, want 1s", got)
	}
	if got := retryDelay(9, nil); got != 8*time.Second {
		t.Errorf("retryDelay(9, nil) = %s, want the 8s cap", got)
	}
	// 服务端的 Retry-After 优先于本地退避，但不接受它把我们挂太久。
	hinted := &APIError{Status: http.StatusTooManyRequests, retryAfter: 3 * time.Second}
	if got := retryDelay(1, hinted); got != 3*time.Second {
		t.Errorf("retryDelay with a 3s hint = %s, want 3s", got)
	}
	absurd := &APIError{Status: http.StatusTooManyRequests, retryAfter: time.Hour}
	if got := retryDelay(1, absurd); got != 30*time.Second {
		t.Errorf("retryDelay with a 1h hint = %s, want the 30s cap", got)
	}
}

func TestRetriableStatus(t *testing.T) {
	retriable := []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	for _, status := range retriable {
		if !retriableStatus(status) {
			t.Errorf("retriableStatus(%d) = false, want true", status)
		}
	}
	// 4xx 是客户端自己的问题，重试只是浪费一次调用。
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusNotFound, http.StatusConflict} {
		if retriableStatus(status) {
			t.Errorf("retriableStatus(%d) = true, want false", status)
		}
	}
}

func TestOnResponseSeesRawSuccessBody(t *testing.T) {
	// --json 靠这个回调做无损透传：服务端新增的字段不能被客户端结构体过滤掉。
	const raw = `{"object":"credit_summary","wallet_available_quota":1,"key_available_quota":2,"future_field":"keep me"}`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, raw)
	})
	client := newTestClient(t, mux)

	var seen string
	client.OnResponse = func(body []byte) { seen = string(body) }
	if _, err := client.Balance(context.Background()); err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if seen != raw {
		t.Errorf("OnResponse body = %q, want the untouched %q", seen, raw)
	}
}

func TestNonJSONSuccessBodyBecomesAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage/token/balance", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>captive portal</html>")
	})
	client := newTestClient(t, mux)

	_, err := client.Balance(context.Background())
	unexpected := AsUnexpectedResponseError(err)
	if unexpected == nil {
		t.Fatalf("Balance error = %v (%T), want *UnexpectedResponseError", err, err)
	}
	if unexpected.Status != http.StatusOK || unexpected.ContentType == "" {
		t.Errorf("response metadata = %#v, want status and content type", unexpected)
	}
	if !strings.Contains(unexpected.BodyPreview, "captive portal") {
		t.Errorf("BodyPreview = %q, want it to keep the raw payload", unexpected.BodyPreview)
	}
}

func TestUnexpectedSuccessResponseExtractsTaskIDAndRequestID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_header")
		io.WriteString(w, `{"task_id":"task_resume","status":"queued"}`)
	})
	client := newTestClient(t, mux)
	_, err := client.Models(context.Background())
	unexpected := AsUnexpectedResponseError(err)
	if unexpected == nil {
		t.Fatalf("Models error = %v (%T), want *UnexpectedResponseError", err, err)
	}
	if unexpected.TaskID != "task_resume" || unexpected.StatusValue != "queued" {
		t.Errorf("extracted task = %q/%q, want task_resume/queued", unexpected.TaskID, unexpected.StatusValue)
	}
	if unexpected.RequestID != "req_header" {
		t.Errorf("RequestID = %q, want req_header", unexpected.RequestID)
	}
	if !strings.Contains(err.Error(), "verge task get task_resume") {
		t.Errorf("error = %q, want resume hint", err)
	}
}

func TestUnexpectedSuccessResponseWithoutTaskIDIsExplicitlyUnrecoverable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"unexpected":true}`)
	})
	client := newTestClient(t, mux)
	_, err := client.Models(context.Background())
	unexpected := AsUnexpectedResponseError(err)
	if unexpected == nil || unexpected.TaskID != "" {
		t.Fatalf("Models error = %#v, want unexpected response without task id", err)
	}
	if !strings.Contains(err.Error(), "task_id not provided") {
		t.Errorf("error = %q, want missing task_id diagnosis", err)
	}
}

func TestUnexpectedSuccessResponseRedactsSensitiveValuesAndLimitsPreview(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "Authorization: Bearer sk-secret https://storage.example/signed?sig=secret "+strings.Repeat("x", 5000))
	})
	client := newTestClient(t, mux)
	_, err := client.Models(context.Background())
	unexpected := AsUnexpectedResponseError(err)
	if unexpected == nil {
		t.Fatalf("Models error = %v (%T), want *UnexpectedResponseError", err, err)
	}
	if len(unexpected.BodyPreview) > maxBodyPreviewBytes {
		t.Errorf("preview length = %d, want <= %d", len(unexpected.BodyPreview), maxBodyPreviewBytes)
	}
	for _, secret := range []string{"sk-secret", "sig=secret", "Authorization:"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %s", secret, err)
		}
	}
}

func TestContextCancellationIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	})
	client := newTestClient(t, mux)
	client.MaxRetries = 3

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if _, err := client.Models(ctx); err == nil {
		t.Fatal("Models should fail once the context is cancelled")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1: a cancelled context must stop the retry loop", got)
	}
}

// decodeJSON is a tiny helper so the wire-format tests read as assertions on JSON keys.
func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, raw)
	}
	return out
}
