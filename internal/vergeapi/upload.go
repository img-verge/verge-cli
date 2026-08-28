package vergeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// UploadFailureRequest reports a failed direct upload for server-side diagnostics.
type UploadFailureRequest struct {
	Code       string `json:"code,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Phase      string `json:"phase,omitempty"`
}

// UploadOptions configures retries and tracing for a presigned PUT upload.
type UploadOptions struct {
	MaxRetries int
	Trace      func(method, urlStr string, status int, elapsed time.Duration)
}

// PutFile streams a local file to a presigned PUT URL and returns its ETag.
// It keeps the historical single-attempt behavior for callers that do not pass options.
func PutFile(ctx context.Context, httpClient *http.Client, putURL, contentType, path string) (string, error) {
	return PutFileWithOptions(ctx, httpClient, putURL, contentType, path, UploadOptions{})
}

// PutFileWithOptions uploads a local file to object storage. Only transport failures
// and the standard retryable statuses are retried; the Verge API key is never sent.
func PutFileWithOptions(ctx context.Context, httpClient *http.Client, putURL, contentType, path string, options UploadOptions) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", path)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("%s is empty", path)
	}
	return putBody(ctx, httpClient, putURL, contentType, info.Name(), info.Size(), options,
		func() (io.ReadCloser, error) {
			// 每次重试都重新打开文件，从字节零开始，避免分块传输和半读状态。
			return os.Open(path)
		})
}

// PutBytes uploads an in-memory payload (typically a re-encoded JPEG) to a presigned
// PUT URL and returns its ETag, with the same retry semantics as PutFileWithOptions.
// name is used in error messages; data must be non-empty.
func PutBytes(ctx context.Context, httpClient *http.Client, putURL, contentType, name string, data []byte) (string, error) {
	return PutBytesWithOptions(ctx, httpClient, putURL, contentType, name, data, UploadOptions{})
}

// PutBytesWithOptions is PutBytes with explicit retry/trace options. The payload lives
// in memory, so retries resend the exact same bytes instead of reopening a file.
func PutBytesWithOptions(ctx context.Context, httpClient *http.Client, putURL, contentType, name string, data []byte, options UploadOptions) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("%s is empty", name)
	}
	return putBody(ctx, httpClient, putURL, contentType, name, int64(len(data)), options,
		func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		})
}

// putBody is the shared presigned-PUT retry loop. openBody produces the request body
// once per attempt; name appears in error messages and the request carries an explicit
// Content-Length because most presigned targets reject chunked transfer.
func putBody(ctx context.Context, httpClient *http.Client, putURL, contentType, name string, contentLength int64, options UploadOptions, openBody func() (io.ReadCloser, error)) (string, error) {
	if strings.TrimSpace(putURL) == "" {
		return "", errors.New("empty upload URL")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Minute}
	}
	maxRetries := options.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, retryDelay(attempt, lastErr)); err != nil {
				return "", err
			}
		}

		body, err := openBody()
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, body)
		if err != nil {
			body.Close()
			return "", fmt.Errorf("build upload request: %w", err)
		}
		req.Header.Set("Content-Type", contentType)
		req.ContentLength = contentLength

		started := time.Now()
		resp, requestErr := httpClient.Do(req)
		if requestErr != nil {
			body.Close()
			if options.Trace != nil {
				options.Trace(http.MethodPut, putURL, 0, time.Since(started))
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			lastErr = fmt.Errorf("upload %s: %w", name, requestErr)
			continue
		}

		// The HTTP client consumes and closes the request body; retries reopen the file
		// from byte zero on the next attempt.
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		respCloseErr := resp.Body.Close()
		if options.Trace != nil {
			options.Trace(http.MethodPut, putURL, resp.StatusCode, time.Since(started))
		}
		if readErr != nil {
			lastErr = fmt.Errorf("read upload response: %w", readErr)
			if resp.StatusCode >= 500 || retriableStatus(resp.StatusCode) {
				continue
			}
			return "", lastErr
		}
		if respCloseErr != nil {
			return "", fmt.Errorf("close upload response: %w", respCloseErr)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			uploadErr := &UploadError{
				Status:  resp.StatusCode,
				Path:    name,
				Message: strings.TrimSpace(string(respBody)),
			}
			if retriableStatus(resp.StatusCode) {
				lastErr = uploadErr
				continue
			}
			return "", uploadErr
		}

		// A successful PUT without ETag cannot be submitted safely.
		etag := strings.Trim(resp.Header.Get("ETag"), `"`)
		if etag == "" {
			return "", &UploadError{
				Status:  resp.StatusCode,
				Path:    name,
				Message: "upload succeeded but no ETag header was returned",
			}
		}
		return etag, nil
	}
	return "", lastErr
}

// UploadError describes a failed PUT to a presigned URL.
type UploadError struct {
	Status  int
	Path    string
	Message string
}

func (e *UploadError) Error() string {
	head := fmt.Sprintf("upload %s failed with HTTP %d", e.Path, e.Status)
	if e.Message != "" {
		return head + ": " + e.Message
	}
	return head
}

// ReportUploadFailure tells the server a direct upload failed.
// POST /images/tasks/uploads/{upload_id}/fail is best-effort diagnostics; it does not
// immediately release the upload session. The server cleans up abandoned sessions.
func (c *Client) ReportUploadFailure(ctx context.Context, uploadID string, req UploadFailureRequest) error {
	if strings.TrimSpace(uploadID) == "" {
		return errors.New("missing upload id")
	}
	path := "/images/tasks/uploads/" + url.PathEscape(uploadID) + "/fail"
	return c.post(ctx, path, req, nil)
}

// UploadStatusCode maps a failed PUT status onto the Verge API error code the server
// expects in ReportUploadFailure.
func UploadStatusCode(status int) string {
	switch {
	case status == http.StatusForbidden:
		return CodeUploadSessionForbidden
	case status == http.StatusNotFound || status == http.StatusGone:
		return CodeUploadSessionInvalid
	case status == http.StatusRequestEntityTooLarge:
		return CodeReferenceTooLarge
	case status >= 500:
		return CodeImageServiceUnavailable
	default:
		return CodeReferenceUnavailable
	}
}
