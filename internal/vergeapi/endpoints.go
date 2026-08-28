package vergeapi

import (
	"context"
	"net/url"
	"strconv"
)

// Models returns the models the current API key may actually use, after user, group
// and per-key model restrictions. GET /models
func (c *Client) Models(ctx context.Context) (*ModelList, error) {
	var out ModelList
	if err := c.get(ctx, "/models", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Balance returns the wallet and per-key remaining quota. GET /usage/token/balance
//
// 只读接口：Key 即使额度耗尽也能查，但 Key 必须存在且所属用户处于启用状态。
func (c *Client) Balance(ctx context.Context) (*Balance, error) {
	var out Balance
	if err := c.get(ctx, "/usage/token/balance", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImageQuotaParams selects which parameter set to price.
type ImageQuotaParams struct {
	Model       string
	Resolution  string
	AspectRatio string
	// N 为 0 时不发这个参数，由服务端按默认值 1 计价。
	N int
}

// ImageQuota returns the quota a given parameter set would pre-charge, using the same
// pricing path as a real submission. GET /usage/token/image-quota
func (c *Client) ImageQuota(ctx context.Context, params ImageQuotaParams) (*ImageQuota, error) {
	query := url.Values{}
	if params.Model != "" {
		query.Set("model", params.Model)
	}
	if params.Resolution != "" {
		query.Set("resolution", params.Resolution)
	}
	if params.AspectRatio != "" {
		query.Set("aspect_ratio", params.AspectRatio)
	}
	if params.N != 0 {
		query.Set("n", strconv.Itoa(params.N))
	}
	var out ImageQuota
	if err := c.get(ctx, "/usage/token/image-quota", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateTask creates an async task directly. POST /images/tasks
//
// 只支持公网 URL 参考图；本地图片必须走 Prepare -> PutUpload -> Submit 三段式。
// 返回的 quota 在创建阶段可能是 0，不要把创建响应当成最终结果。
func (c *Client) CreateTask(ctx context.Context, req CreateTaskRequest) (*Task, error) {
	var out Task
	if err := c.post(ctx, "/images/tasks", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Prepare asks for presigned upload URLs and creates an `uploading` task.
// POST /images/tasks/prepare
//
// 只为需要直传的本地图片申请；公网 URL 不需要 put_url，也不能出现在这个请求里。
func (c *Client) Prepare(ctx context.Context, req PrepareRequest) (*PrepareResponse, error) {
	var out PrepareResponse
	if err := c.post(ctx, "/images/tasks/prepare", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Submit commits the `uploading` task created by Prepare. POST /images/tasks/submit
func (c *Client) Submit(ctx context.Context, req SubmitRequest) (*Task, error) {
	var out Task
	if err := c.post(ctx, "/images/tasks/submit", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTask fetches a task by ID. GET /images/tasks/{task_id}
//
// HTTP 200 不代表成功：status 仍可能是 failed，此时 data 缺失、error 字段给出原因。
func (c *Client) GetTask(ctx context.Context, taskID string) (*Task, error) {
	var out Task
	if err := c.get(ctx, "/images/tasks/"+url.PathEscape(taskID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
