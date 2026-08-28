package vergeapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestPutFileUsesPresignedSemantics locks the three properties a presigned PUT depends on:
// no credentials of ours on a third-party host, a real Content-Length instead of chunked
// transfer, and the Content-Type prepare was told about.
func TestPutFileUsesPresignedSemantics(t *testing.T) {
	var (
		auth             string
		contentType      string
		contentLength    int64
		transferEncoding []string
		body             []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		auth = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		contentLength = r.ContentLength
		transferEncoding = r.TransferEncoding
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	content := []byte("pretend PNG bytes")
	path := writeFile(t, "a.png", content)

	etag, err := PutFile(context.Background(), server.Client(), server.URL+"/put/1", "image/png", path)
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	// submit 需要裸 ETag，对象存储回的是带引号的。
	if etag != "d41d8cd98f00b204e9800998" {
		t.Errorf("etag = %q, want the quotes stripped", etag)
	}
	if auth != "" {
		t.Errorf("Authorization = %q; a presigned URL points at object storage, sending our API key there leaks it", auth)
	}
	if contentType != "image/png" {
		t.Errorf("Content-Type = %q, want image/png: the signature covers it", contentType)
	}
	if contentLength != int64(len(content)) {
		t.Errorf("Content-Length = %d, want %d", contentLength, len(content))
	}
	if len(transferEncoding) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none: most presigned PUT targets reject chunked uploads", transferEncoding)
	}
	if string(body) != string(content) {
		t.Errorf("uploaded body = %q, want %q", body, content)
	}
}

func TestPutFileRetriesRetryableStoreResponses(t *testing.T) {
	var attempts int
	var bodies [][]byte
	var auths []string
	var lengths []int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		auths = append(auths, r.Header.Get("Authorization"))
		lengths = append(lengths, r.ContentLength)
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"retry-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	content := []byte("retryable upload body")
	path := writeFile(t, "retry.png", content)
	var traces []int
	etag, err := PutFileWithOptions(context.Background(), server.Client(), server.URL, "image/png", path, UploadOptions{
		MaxRetries: 1,
		Trace: func(method, target string, status int, _ time.Duration) {
			if method != http.MethodPut || target != server.URL {
				t.Errorf("trace = %s %s, want PUT %s", method, target, server.URL)
			}
			traces = append(traces, status)
		},
	})
	if err != nil {
		t.Fatalf("PutFileWithOptions: %v", err)
	}
	if etag != "retry-etag" {
		t.Errorf("etag = %q, want retry-etag", etag)
	}
	if attempts != 2 || len(bodies) != 2 {
		t.Fatalf("attempts = %d, bodies = %d, want two attempts", attempts, len(bodies))
	}
	for i, body := range bodies {
		if string(body) != string(content) {
			t.Errorf("attempt %d body = %q, want %q", i+1, body, content)
		}
		if auths[i] != "" {
			t.Errorf("attempt %d Authorization = %q, want empty", i+1, auths[i])
		}
		if lengths[i] != int64(len(content)) {
			t.Errorf("attempt %d Content-Length = %d, want %d", i+1, lengths[i], len(content))
		}
	}
	if len(traces) != 2 || traces[0] != http.StatusServiceUnavailable || traces[1] != http.StatusOK {
		t.Errorf("traces = %v, want [503 200]", traces)
	}
}

func TestPutFileDoesNotRetryNonRetryableStoreResponses(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "denied")
	}))
	defer server.Close()

	_, err := PutFileWithOptions(context.Background(), server.Client(), server.URL, "image/png", writeFile(t, "denied.png", []byte("x")), UploadOptions{MaxRetries: 3})
	if err == nil {
		t.Fatal("PutFileWithOptions should fail")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want one non-retryable attempt", attempts)
	}
}

func TestPutFileRejectsResponsesWithoutETag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// 没有 ETag 就没法 submit，此时"上传成功"是假的，必须当成失败。
	_, err := PutFile(context.Background(), server.Client(), server.URL, "image/png", writeFile(t, "a.png", []byte("x")))
	if err == nil {
		t.Fatal("PutFile should fail when the store returns no ETag")
	}
}

func TestPutFileReportsHTTPFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
	}))
	defer server.Close()

	_, err := PutFile(context.Background(), server.Client(), server.URL, "image/png", writeFile(t, "a.png", []byte("x")))
	var uploadErr *UploadError
	if !errors.As(err, &uploadErr) {
		t.Fatalf("error = %v (%T), want *UploadError", err, err)
	}
	if uploadErr.Status != http.StatusForbidden {
		t.Errorf("Status = %d, want 403", uploadErr.Status)
	}
	if uploadErr.Message == "" {
		t.Error("the store's response body is the only clue about why it refused; keep it")
	}
}

func TestPutFileValidatesItsInputs(t *testing.T) {
	if _, err := PutFile(context.Background(), nil, "", "image/png", "a.png"); err == nil {
		t.Error("an empty upload URL should fail before any request")
	}
	if _, err := PutFile(context.Background(), nil, "https://store.test/put", "image/png", writeFile(t, "empty.png", nil)); err == nil {
		t.Error("an empty file should fail before any request")
	}
	if _, err := PutFile(context.Background(), nil, "https://store.test/put", "image/png", filepath.Join(t.TempDir(), "missing.png")); err == nil {
		t.Error("a missing file should fail before any request")
	}
}

// TestUploadStatusCode maps store failures onto the codes the fail endpoint expects, so
// the server can tell "the session is gone" from "the file is too big".
func TestUploadStatusCode(t *testing.T) {
	tests := map[int]string{
		http.StatusForbidden:             CodeUploadSessionForbidden,
		http.StatusNotFound:              CodeUploadSessionInvalid,
		http.StatusGone:                  CodeUploadSessionInvalid,
		http.StatusRequestEntityTooLarge: CodeReferenceTooLarge,
		http.StatusInternalServerError:   CodeImageServiceUnavailable,
		http.StatusBadGateway:            CodeImageServiceUnavailable,
		http.StatusBadRequest:            CodeReferenceUnavailable,
		// 0 表示连响应都没拿到（网络中断、Ctrl-C），也要有个能上报的 code。
		0: CodeReferenceUnavailable,
	}
	for status, want := range tests {
		if got := UploadStatusCode(status); got != want {
			t.Errorf("UploadStatusCode(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestReportUploadFailureWireFormat(t *testing.T) {
	var path string
	var captured []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/tasks/uploads/{upload_id}/fail", func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		captured, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"success":true}`)
	})
	client := newTestClient(t, mux)

	err := client.ReportUploadFailure(context.Background(), "up 1", UploadFailureRequest{
		Code:       CodeUploadSessionForbidden,
		HTTPStatus: http.StatusForbidden,
		Phase:      "upload",
	})
	if err != nil {
		t.Fatalf("ReportUploadFailure: %v", err)
	}
	if path != "/v1/images/tasks/uploads/up%201/fail" {
		t.Errorf("path = %q, want the upload id escaped", path)
	}
	body := decodeJSON(t, captured)
	if body["code"] != CodeUploadSessionForbidden {
		t.Errorf("code = %v, want %q", body["code"], CodeUploadSessionForbidden)
	}
	if body["http_status"] != float64(http.StatusForbidden) {
		t.Errorf("http_status = %v, want 403", body["http_status"])
	}
	if body["phase"] != "upload" {
		t.Errorf("phase = %v, want upload", body["phase"])
	}
}

func TestReportUploadFailureNeedsAnUploadID(t *testing.T) {
	client := newTestClient(t, http.NewServeMux())
	if err := client.ReportUploadFailure(context.Background(), "  ", UploadFailureRequest{}); err == nil {
		t.Error("an empty upload id should fail locally instead of POSTing to a bad path")
	}
}
