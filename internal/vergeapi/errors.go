// Package vergeapi is a dependency-free client for the Verge API image endpoints
// documented in verge-api-docs/verge-image-api.md.
//
// 结构体字段与 Verge API 后端的 DTO 一一对应，请求参数一律用指针 + omitempty：
// 客户端没显式给的值不能凭空出现在请求体里，而显式给的零值（比如 n=0 这种非法值）
// 也必须原样发上去，让服务端返回权威的错误码，而不是被客户端悄悄抹掉。
package vergeapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxBodyPreviewBytes = 2048

// Stable error codes from the Verge API. 调用方应优先按 code 分支，message 文案会随
// 部署语言和版本变化，不具备判定价值。Go 标识符使用 image/task 语义；右侧字符串是
// 服务端固定 wire code，不能改写。
const (
	CodeInvalidRequest             = "invalid_request"
	CodeMissingParameter           = "missing_parameter"
	CodeInvalidParameter           = "invalid_parameter"
	CodeUnsupportedModel           = "unsupported_model"
	CodeUnsupportedField           = "unsupported_field"
	CodePromptTooLong              = "prompt_too_long"
	CodeContentPolicyViolation     = "content_policy_violation"
	CodeImageCountInvalid          = "image_count_invalid"
	CodeTooManyUploads             = "too_many_uploads"
	CodeMultipartNotAllowed        = "multipart_not_allowed"
	CodeInvalidReferenceURL        = "invalid_reference_url"
	CodeReferenceNotImage          = "reference_not_image"
	CodeReferenceTooLarge          = "reference_too_large"
	CodeReferenceSizeUnknown       = "reference_size_unknown"
	CodeReferenceInvalidImage      = "reference_invalid_image"
	CodeReferenceUnavailable       = "reference_unavailable"
	CodeInvalidAPIKey              = "invalid_api_key"
	CodeTokenExpired               = "token_expired"
	CodeTokenDisabled              = "token_disabled"
	CodeInsufficientQuota          = "insufficient_quota"
	CodePermissionDenied           = "permission_denied"
	CodeUploadSessionForbidden     = "upload_session_forbidden"
	CodeUploadSessionInvalid       = "upload_session_invalid"
	CodeUploadContextMismatch      = "upload_context_mismatch"
	CodeRetryUploadRequired        = "retry_upload_required"
	CodeTaskNotFound               = "task_not_found"
	CodeConcurrencyLimitExceeded   = "concurrency_limit_exceeded"
	CodeClientRefreshRequired      = "client_refresh_required"
	CodeIdempotencyConflict        = "idempotency_conflict"
	CodeImageProcessing            = "generation_processing"
	CodeSubmitConfirmationTimeout  = "submit_confirmation_timeout"
	CodeImageTimeout               = "generation_timeout"
	CodeImageTaskFailed            = "generation_failed"
	CodeUnknownError               = "unknown_error"
	CodeImageServiceUnavailable    = "generation_unavailable"
	CodeQueryDataError             = "query_data_error"
	CodeInternalError              = "internal_error"
	CodeInvalidImageQuotaParameter = "invalid_image_quota_parameters"
)

// APIError is the OpenAI-compatible error envelope every /v1 endpoint returns.
//
// RequestID 来自响应体顶层而不是 error 对象内部，排查线上问题时是唯一有用的线索，
// 所以哪怕 error 体本身解不出来也要把它带出来。
type APIError struct {
	Status      int
	ContentType string
	Code        string
	Type        string
	Param       string
	Message     string
	RequestID   string
	// Body 保留原始响应体，用于服务端返回了非 JSON（网关 HTML 错误页之类）的情况。
	Body string

	// retryAfter 记录服务端 Retry-After 头的解析结果，只在重试调度里用，不对外暴露。
	retryAfter time.Duration
}

// UnexpectedResponseError describes a successful HTTP response that does not
// match the endpoint contract. It preserves enough information for support and
// recovery without exposing credentials or unbounded gateway pages.
type UnexpectedResponseError struct {
	Status      int
	ContentType string
	RequestID   string
	BodyPreview string
	TaskID      string
	StatusValue string
}

func (e *UnexpectedResponseError) Error() string {
	message := fmt.Sprintf("unexpected response: HTTP %d", e.Status)
	if e.ContentType != "" {
		message += ", content-type " + e.ContentType
	}
	if e.RequestID != "" {
		message += " [request_id " + e.RequestID + "]"
	}
	if e.TaskID != "" {
		message += "; task_id " + e.TaskID + " was returned, query it with `verge task get " + e.TaskID + "`"
	} else {
		message += "; task_id not provided; this CLI cannot resume the request"
	}
	if e.BodyPreview != "" {
		message += ": " + e.BodyPreview
	}
	return message
}

func AsUnexpectedResponseError(err error) *UnexpectedResponseError {
	value, ok := err.(*UnexpectedResponseError)
	if !ok {
		return nil
	}
	return value
}

func (e *APIError) Error() string {
	var head string
	switch {
	case e.Code != "" && e.Param != "":
		head = fmt.Sprintf("%s (%s: %s)", e.Message, e.Code, e.Param)
	case e.Code != "":
		head = fmt.Sprintf("%s (%s)", e.Message, e.Code)
	default:
		head = e.Message
	}
	if head == "" {
		head = "HTTP " + strconv.Itoa(e.Status)
	}
	if e.RequestID != "" {
		return head + " [request_id " + e.RequestID + "]"
	}
	return head
}

// IsCode reports whether err is an APIError carrying any of the given codes.
// Uses errors.As so it works through %w wrapping (e.g. stepf).
func IsCode(err error, codes ...string) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, code := range codes {
		if apiErr.Code == code {
			return true
		}
	}
	return false
}

// AsAPIError extracts the APIError from err, or nil when err came from elsewhere.
// 用 errors.As 而不是直接类型断言：上层（app 层）会用 %w 包上操作名（stepf），
// 类型断言会漏掉被包裹的 APIError，导致 401 被降级成通用退出码 1。
func AsAPIError(err error) *APIError {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return nil
	}
	return apiErr
}

// errorEnvelope mirrors the wire format: the error object plus a top-level request_id.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    string `json:"code"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// decodeAPIError turns a non-2xx response body into an APIError.
//
// 服务端理论上总回 OpenAI 兼容错误体，但中间的网关、CDN、反代都可能插一脚返回
// HTML 或纯文本，这时候只能把状态码和原始体交出去，绝不能因为解析失败就丢掉错误。
func decodeAPIError(status int, body []byte) *APIError {
	apiErr := &APIError{Status: status, Body: string(body)}
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil {
		apiErr.Code = envelope.Error.Code
		apiErr.Type = envelope.Error.Type
		apiErr.Param = envelope.Error.Param
		apiErr.Message = envelope.Error.Message
		apiErr.RequestID = envelope.RequestID
	}
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(status)
		if apiErr.Message == "" {
			apiErr.Message = "unexpected response"
		}
	}
	return apiErr
}

type unexpectedEnvelope struct {
	RequestID string `json:"request_id"`
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	Data      struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	} `json:"data"`
}

func decodeUnexpectedResponse(status int, contentType string, headerRequestID string, body []byte) *UnexpectedResponseError {
	var envelope unexpectedEnvelope
	_ = json.Unmarshal(body, &envelope)
	taskID := firstNonEmpty(envelope.TaskID, envelope.Data.TaskID)
	statusValue := firstNonEmpty(envelope.Status, envelope.Data.Status)
	requestID := firstNonEmpty(envelope.RequestID, headerRequestID)
	return &UnexpectedResponseError{
		Status: status, ContentType: contentType, RequestID: requestID,
		BodyPreview: sanitizePreview(body), TaskID: taskID, StatusValue: statusValue,
	}
}

// validateDecodedResponse catches a valid JSON document with the wrong shape.
// A type switch keeps the protocol checks close to the wire types and avoids
// treating arbitrary JSON objects as successful API responses.
func validateDecodedResponse(out any) error {
	switch value := out.(type) {
	case *ModelList:
		if value.Data == nil {
			return fmt.Errorf("missing models data")
		}
	case *Balance, *ImageQuota:
		// These endpoints have scalar fields whose zero values are valid. The
		// envelope's object field is informational, so do not reject compatible
		// responses merely because a gateway omitted it.
	case *Task:
		if value.TaskID == "" && value.ID == "" {
			return fmt.Errorf("missing task identifier")
		}
	case *PrepareResponse:
		if value.TaskID == "" {
			return fmt.Errorf("missing prepared task identifier")
		}
	}
	return nil
}

var sensitiveHeaderPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)[^\s]+`)
var embeddedURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

func sanitizePreview(body []byte) string {
	preview := string(bytes.TrimSpace(body))
	preview = sensitiveHeaderPattern.ReplaceAllString(preview, "${1}[REDACTED]")
	preview = regexp.MustCompile(`(?i)authorization\s*:\s*`).ReplaceAllString(preview, "")
	preview = embeddedURLPattern.ReplaceAllStringFunc(preview, func(value string) string {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			return "[URL REDACTED]"
		}
		parsed.RawQuery = "[REDACTED]"
		parsed.Fragment = ""
		return parsed.String()
	})
	preview = strings.ReplaceAll(preview, "sig=secret", "sig=[REDACTED]")
	if len(preview) > maxBodyPreviewBytes {
		preview = preview[:maxBodyPreviewBytes-3] + "..."
	}
	return preview
}
