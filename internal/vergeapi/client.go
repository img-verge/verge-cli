package vergeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the public Verge API endpoint, including the required /v1 suffix.
const DefaultBaseURL = "https://api.verge-ai.xyz/v1"

// maxErrorBodyBytes caps how much of an error response we read. 网关出错时可能回一整个
// HTML 页面，没必要整份读进内存。
const maxErrorBodyBytes = 64 << 10

// Client talks to the Verge API /v1 endpoints.
//
// 零第三方依赖，只用 net/http。除 lastRequestID（每次响应后更新）外，所有字段在
// New 之后只读；lastRequestID 供同一命令内的顺序调用读取。
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	UserAgent  string

	// MaxRetries 只作用于幂等请求（GET 与直传 PUT）。POST 一律不重试：
	// submit_unknown 表示提交结果未确认，客户端必须查询已有 task_id，不能重复提交；
	// 计费在后续提交/处理阶段结算，不能通过重试 POST 来“恢复”。
	MaxRetries int

	// Trace 非空时，每次请求都会被回调一次，用于 --verbose。
	Trace func(method, urlStr string, status int, elapsed time.Duration)

	// OnResponse 非空时，每个 2xx 响应体在解码前原样回调一次。
	// --json 用它做无损透传：服务端将来新增字段也不会被客户端的结构体过滤掉。
	OnResponse func(body []byte)

	// lastRequestID 记录最近一次响应的请求 ID，成功路径的人类可读输出用它透出。
	lastRequestID string
}

// Options configures a Client.
type Options struct {
	BaseURL    string
	APIKey     string
	Timeout    time.Duration
	UserAgent  string
	MaxRetries int
	Trace      func(method, urlStr string, status int, elapsed time.Duration)
}

// New builds a Client. BaseURL 会被规范化到带 /v1 的形式。
func New(opts Options) (*Client, error) {
	base, err := NormalizeBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("missing API key")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	retries := opts.MaxRetries
	if retries < 0 {
		retries = 0
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = "verge-cli"
	}
	return &Client{
		BaseURL:    base,
		APIKey:     strings.TrimSpace(opts.APIKey),
		HTTPClient: &http.Client{Timeout: timeout},
		UserAgent:  userAgent,
		MaxRetries: retries,
		Trace:      opts.Trace,
	}, nil
}

// NormalizeBaseURL trims trailing slashes and appends /v1 when it is missing.
//
// 文档要求 Base URL 必须包含 /v1，但用户几乎一定会只填域名。与其让每个接口都回
// 404 让人自己猜，不如在这里补齐；已经带 /v1 的（含自建网关的 /gateway/v1）保持原样。
func NormalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = DefaultBaseURL
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid base URL %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid base URL %q: scheme must be http or https", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("invalid base URL %q: missing host", raw)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/v1") {
		parsed.Path += "/v1"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// get performs an idempotent GET and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, target, nil, out, true)
}

// post performs a non-idempotent POST and decodes the JSON body into out.
func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request body: %w", err)
	}
	return c.do(ctx, http.MethodPost, c.BaseURL+path, payload, out, false)
}

// do runs a request with retries when idempotent, then decodes or converts the result.
func (c *Client) do(ctx context.Context, method, target string, body []byte, out any, idempotent bool) error {
	attempts := 1
	if idempotent {
		attempts += c.MaxRetries
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, retryDelay(attempt, lastErr)); err != nil {
				return err
			}
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.UserAgent)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		started := time.Now()
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			if c.Trace != nil {
				c.Trace(method, target, 0, time.Since(started))
			}
			// 传输层失败（连不上、TLS、读超时）对幂等请求可以重试；ctx 取消不重试。
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("request %s %s: %w", method, target, err)
			continue
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		closeErr := resp.Body.Close()
		if c.Trace != nil {
			c.Trace(method, target, resp.StatusCode, time.Since(started))
		}
		if readErr != nil {
			lastErr = fmt.Errorf("read response: %w", readErr)
			continue
		}
		if closeErr != nil && lastErr == nil {
			lastErr = fmt.Errorf("close response: %w", closeErr)
		}

		// 成功和错误都记录请求 ID：错误输出与成功输出都需要它做排查线索。
		c.lastRequestID = requestIDFromHeader(resp.Header)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if c.OnResponse != nil {
				c.OnResponse(payload)
			}
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(payload, out); err != nil {
				return decodeUnexpectedResponse(resp.StatusCode, resp.Header.Get("Content-Type"), requestIDFromHeader(resp.Header), payload)
			}
			if err := validateDecodedResponse(out); err != nil {
				return decodeUnexpectedResponse(resp.StatusCode, resp.Header.Get("Content-Type"), requestIDFromHeader(resp.Header), payload)
			}
			return nil
		}

		apiErr := decodeAPIError(resp.StatusCode, payload)
		apiErr.ContentType = resp.Header.Get("Content-Type")
		apiErr.RequestID = firstNonEmpty(apiErr.RequestID, requestIDFromHeader(resp.Header))
		if !retriableStatus(resp.StatusCode) {
			return apiErr
		}
		apiErr.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		lastErr = apiErr
	}
	return lastErr
}

// retryAfter is carried on APIError so retryDelay can honour the server's hint without
// widening the exported surface.
func (e *APIError) retryAfterHint() time.Duration { return e.retryAfter }

// retriableStatus reports whether a status is worth retrying for idempotent requests.
func retriableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryDelay is exponential backoff capped at 8s, overridden by any Retry-After hint.
func retryDelay(attempt int, lastErr error) time.Duration {
	if apiErr := AsAPIError(lastErr); apiErr != nil {
		if hint := apiErr.retryAfterHint(); hint > 0 {
			if hint > 30*time.Second {
				return 30 * time.Second
			}
			return hint
		}
	}
	backoff := time.Duration(math.Pow(2, float64(attempt-1))) * 500 * time.Millisecond
	if backoff > 8*time.Second {
		return 8 * time.Second
	}
	return backoff
}

// parseRetryAfter accepts both the delta-seconds and the HTTP-date forms.
func parseRetryAfter(value string) time.Duration {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(trimmed); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(trimmed); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

// sleepCtx waits for d unless ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// requestIDFromHeader extracts the request id every response carries. 真实网关回
// X-Oneapi-Request-Id（文档未记录，实测确认）；自建部署或旧网关可能回 X-Request-Id，
// 两个都认。错误路径优先取 body 顶层 request_id，这里只是兜底。
func requestIDFromHeader(header http.Header) string {
	return firstNonEmpty(header.Get("X-Oneapi-Request-Id"), header.Get("X-Request-Id"))
}

// LastRequestID returns the request id of the most recent response, or "" when the
// server did not return one. 成功路径的人类可读输出用它透出，供支持排查。
func (c *Client) LastRequestID() string { return c.lastRequestID }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
