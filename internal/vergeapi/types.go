package vergeapi

import "encoding/json"

// ---------- 公共生图参数 ----------

// ReferenceURL is a public reference image. 服务端两种形式都收：裸字符串或
// {url, name}；带 name 才能在 prompt 里用 [@名称] 引用。
type ReferenceURL struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
}

// MarshalJSON emits the bare string form when there is no name, matching what the
// docs show for the simple case and keeping request bodies minimal.
func (r ReferenceURL) MarshalJSON() ([]byte, error) {
	if r.Name == "" {
		return json.Marshal(r.URL)
	}
	return json.Marshal(struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}{URL: r.URL, Name: r.Name})
}

// ImageData is one result image. b64_json 恒为空串，服务端不回图片 base64。
type ImageData struct {
	URL           string `json:"url"`
	B64JSON       string `json:"b64_json"`
	RevisedPrompt string `json:"revised_prompt"`
	CoverURL      string `json:"cover_url,omitempty"`
}

// ---------- POST /images/tasks ----------

// CreateTaskRequest is the async task creation body. 该接口只接受公网 URL 参考图，
// image / images / base64 / data URL 都会被显式拒绝。
type CreateTaskRequest struct {
	Model       string         `json:"model"`
	Prompt      string         `json:"prompt"`
	Resolution  string         `json:"resolution,omitempty"`
	AspectRatio string         `json:"aspect_ratio,omitempty"`
	N           *int           `json:"n,omitempty"`
	Group       string         `json:"group,omitempty"`
	ImageURLs   []ReferenceURL `json:"image_urls,omitempty"`
}

// ---------- POST /images/tasks/prepare ----------

// PrepareImage describes one local file that needs an upload URL.
// 注意 fileName / contentType 是 camelCase，与同一请求体里 snake_case 的生图参数混用，
// 这是服务端 DTO 的既定口径，不要"顺手统一"。
type PrepareImage struct {
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

// PrepareRequest asks for presigned PUT URLs and creates an `uploading` task.
// ImageCount 是必填且必须等于 len(Images)，用指针以便显式发 0 让服务端回
// image_count_invalid，而不是客户端自己判死。
type PrepareRequest struct {
	Model       string         `json:"model,omitempty"`
	Prompt      string         `json:"prompt,omitempty"`
	Resolution  string         `json:"resolution,omitempty"`
	AspectRatio string         `json:"aspect_ratio,omitempty"`
	N           *int           `json:"n,omitempty"`
	Group       string         `json:"group,omitempty"`
	ImageCount  *int           `json:"imageCount"`
	Images      []PrepareImage `json:"images"`
}

// PrepareUpload is one presigned upload slot. ID 是不透明字符串，必须原样回传。
type PrepareUpload struct {
	ID        string `json:"id"`
	PutURL    string `json:"put_url"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// PrepareResponse carries the created task plus one upload slot per requested image,
// in the same order as PrepareRequest.Images.
type PrepareResponse struct {
	TaskID  string          `json:"task_id"`
	Status  string          `json:"status"`
	Uploads []PrepareUpload `json:"uploads"`
}

// ---------- POST /images/tasks/submit ----------

// SubmitUpload references one uploaded object. ETag 来自 PUT 响应头，
// Name 可选，用于 prompt 里的 [@名称]。
type SubmitUpload struct {
	ID   string `json:"id"`
	ETag string `json:"etag"`
	Name string `json:"name,omitempty"`
}

// SubmitRequest commits an `uploading` task. 所有 uploads 必须来自同一次 prepare。
type SubmitRequest struct {
	TaskID      string         `json:"task_id"`
	Model       string         `json:"model,omitempty"`
	Prompt      string         `json:"prompt"`
	Resolution  string         `json:"resolution,omitempty"`
	AspectRatio string         `json:"aspect_ratio,omitempty"`
	N           *int           `json:"n,omitempty"`
	Group       string         `json:"group,omitempty"`
	Uploads     []SubmitUpload `json:"uploads"`
	ImageURLs   []ReferenceURL `json:"image_urls,omitempty"`
}

// ---------- GET /images/tasks/{task_id} ----------

// Task statuses. 前四个是非终态，调用方应继续轮询。
const (
	StatusUploading     = "uploading"
	StatusQueued        = "queued"
	StatusSubmitUnknown = "submit_unknown"
	StatusInProgress    = "in_progress"
	StatusCompleted     = "completed"
	StatusFailed        = "failed"
)

// IsTerminalStatus reports whether a task will never change status again.
//
// 未知状态一律当成非终态：服务端将来新增中间态时，客户端应该继续轮询而不是
// 直接判定成功或失败。
func IsTerminalStatus(status string) bool {
	return status == StatusCompleted || status == StatusFailed
}

// TaskError is the failure detail present only when status == failed.
type TaskError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    string `json:"code"`
}

// Task is the public task view. Quota 在排队/处理中可能为 0，只有完成后才是本次
// 任务实际消耗的额度。
type Task struct {
	ID          string      `json:"id"`
	TaskID      string      `json:"task_id"`
	Object      string      `json:"object"`
	Status      string      `json:"status"`
	CreatedAt   int64       `json:"created_at"`
	CompletedAt int64       `json:"completed_at,omitempty"`
	Model       string      `json:"model,omitempty"`
	Quota       int         `json:"quota"`
	Data        []ImageData `json:"data,omitempty"`
	Error       *TaskError  `json:"error,omitempty"`
}

// ---------- GET /models ----------

// Model is one entry from GET /models, scoped to what the current key may use.
type Model struct {
	ID                     string   `json:"id"`
	Object                 string   `json:"object"`
	Created                int64    `json:"created"`
	OwnedBy                string   `json:"owned_by"`
	SupportedEndpointTypes []string `json:"supported_endpoint_types"`
}

// SupportsImageEndpoint reports whether the model can be used with the image
// endpoints, per its supported_endpoint_types.
func (m Model) SupportsImageEndpoint() bool {
	for _, endpoint := range m.SupportedEndpointTypes {
		if endpoint == "image-generation" {
			return true
		}
	}
	return false
}

// ModelList is the GET /models envelope.
type ModelList struct {
	Object  string  `json:"object"`
	Success bool    `json:"success"`
	Data    []Model `json:"data"`
}

// ---------- GET /usage/token/balance ----------

// Balance is the credit_summary envelope.
//
// KeyAvailableQuota 用 *int：无限额度 Key 返回 null，有限额度 Key 耗尽返回 0，
// 两者语义完全不同，不能都塌成 0。
type Balance struct {
	Object               string `json:"object"`
	TotalGranted         int    `json:"total_granted"`
	TotalUsed            int    `json:"total_used"`
	TotalAvailable       int    `json:"total_available"`
	ExpiresAt            int64  `json:"expires_at"`
	WalletAvailableQuota int    `json:"wallet_available_quota"`
	KeyAvailableQuota    *int   `json:"key_available_quota"`
}

// Unlimited reports whether this key has no per-key quota cap.
func (b Balance) Unlimited() bool { return b.KeyAvailableQuota == nil }

// ---------- GET /usage/token/image-quota ----------

// ImageQuota is the image_quota envelope: what a given parameter set would pre-charge.
type ImageQuota struct {
	Object           string `json:"object"`
	Model            string `json:"model"`
	Resolution       string `json:"resolution"`
	AspectRatio      string `json:"aspect_ratio"`
	N                int    `json:"n"`
	Quota            int    `json:"quota"`
	PreConsumedQuota int    `json:"pre_consumed_quota"`
}
