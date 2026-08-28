package vergeapi

import (
	"context"
	"fmt"
	"time"
)

// Default polling parameters. 参考图任务文档写明可能跑 3 到 5 分钟，默认上限留到
// 20 分钟，避免正常任务被客户端提前判死。
const (
	DefaultPollInterval    = 2 * time.Second
	DefaultMaxPollInterval = 15 * time.Second
	DefaultWaitTimeout     = 20 * time.Minute
)

// WaitOptions tunes WaitForTask.
type WaitOptions struct {
	Interval    time.Duration
	MaxInterval time.Duration
	Timeout     time.Duration
	// OnPoll 每次拿到任务快照时回调一次（包含第一次），用于打印进度。
	OnPoll func(task *Task, elapsed time.Duration)
}

// WaitTimeoutError means the task was still non-terminal when the deadline hit.
//
// 这不代表任务失败：服务端仍在跑，额度也仍然占着，调用方之后可以拿同一个 task_id
// 继续查。所以要和「任务失败」区分成不同的退出码。
type WaitTimeoutError struct {
	TaskID     string
	LastStatus string
	Elapsed    time.Duration
}

func (e *WaitTimeoutError) Error() string {
	return fmt.Sprintf(
		"timed out after %s waiting for task %s (last status %q); the task is still running, query it again later",
		e.Elapsed.Round(time.Second), e.TaskID, e.LastStatus,
	)
}

// WaitForTask polls GET /images/tasks/{task_id} until the task reaches a terminal
// status, then returns it.
//
// 返回 status == failed 的任务不算错误：失败原因在 task.Error 里，由调用方决定怎么
// 呈现和退出。只有传输失败、API 报错和等待超时才返回 error。
//
// 未知状态按非终态处理，服务端将来新增中间态时客户端会继续轮询而不是误判。
func (c *Client) WaitForTask(ctx context.Context, taskID string, opts WaitOptions) (*Task, error) {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	maxInterval := opts.MaxInterval
	if maxInterval < interval {
		maxInterval = DefaultMaxPollInterval
	}
	if maxInterval < interval {
		maxInterval = interval
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}

	started := time.Now()
	deadline := started.Add(timeout)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	lastStatus := ""
	for {
		task, err := c.GetTask(waitCtx, taskID)
		if err != nil {
			// ctx 到期时 GetTask 返回的是 context.DeadlineExceeded，要翻译成明确的
			// 等待超时，否则用户只看到一句 "context deadline exceeded"。
			if waitCtx.Err() != nil && ctx.Err() == nil {
				return nil, &WaitTimeoutError{
					TaskID:     taskID,
					LastStatus: lastStatus,
					Elapsed:    time.Since(started),
				}
			}
			return nil, err
		}
		lastStatus = task.Status
		if opts.OnPoll != nil {
			opts.OnPoll(task, time.Since(started))
		}
		if IsTerminalStatus(task.Status) {
			return task, nil
		}

		// 下一次轮询若必然落在截止时间之后，就没必要再睡一轮空等。
		if time.Now().Add(interval).After(deadline) {
			if err := sleepCtx(waitCtx, time.Until(deadline)); err != nil && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &WaitTimeoutError{
				TaskID:     taskID,
				LastStatus: lastStatus,
				Elapsed:    time.Since(started),
			}
		}
		if err := sleepCtx(waitCtx, interval); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &WaitTimeoutError{
				TaskID:     taskID,
				LastStatus: lastStatus,
				Elapsed:    time.Since(started),
			}
		}
		interval = nextInterval(interval, maxInterval)
	}
}

// nextInterval grows the poll interval by 1.5x, capped at max.
func nextInterval(current, max time.Duration) time.Duration {
	next := current + current/2
	if next > max {
		return max
	}
	return next
}
