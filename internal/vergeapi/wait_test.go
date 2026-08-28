package vergeapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// statusSequence serves one status per request, repeating the last one forever.
func statusSequence(counter *atomic.Int32, statuses ...string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		index := int(counter.Add(1)) - 1
		if index >= len(statuses) {
			index = len(statuses) - 1
		}
		status := statuses[index]
		if status == StatusCompleted {
			io.WriteString(w, `{"task_id":"task_1","status":"completed","quota":4000,"created_at":100,"completed_at":160,"data":[{"url":"https://cdn.test/1.png"}]}`)
			return
		}
		io.WriteString(w, `{"task_id":"task_1","status":"`+status+`","quota":0}`)
	})
	return mux
}

func TestWaitForTaskPollsUntilTerminal(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, statusSequence(&calls, StatusQueued, StatusInProgress, StatusCompleted))

	var polled []string
	task, err := client.WaitForTask(context.Background(), "task_1", WaitOptions{
		Interval:    time.Millisecond,
		MaxInterval: 2 * time.Millisecond,
		Timeout:     10 * time.Second,
		OnPoll:      func(task *Task, elapsed time.Duration) { polled = append(polled, task.Status) },
	})
	if err != nil {
		t.Fatalf("WaitForTask: %v", err)
	}
	if task.Status != StatusCompleted {
		t.Errorf("Status = %q, want %q", task.Status, StatusCompleted)
	}
	if len(task.Data) != 1 {
		t.Errorf("Data = %v, want the single result image", task.Data)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("polls = %d, want 3", got)
	}
	// 每次拿到快照都要回调一次，包含第一次，否则进度输出会漏掉起始状态。
	want := []string{StatusQueued, StatusInProgress, StatusCompleted}
	if len(polled) != len(want) {
		t.Fatalf("OnPoll statuses = %v, want %v", polled, want)
	}
	for index := range want {
		if polled[index] != want[index] {
			t.Errorf("OnPoll[%d] = %q, want %q", index, polled[index], want[index])
		}
	}
}

// TestWaitForTaskReturnsFailedTaskWithoutError keeps "the task failed" separate from
// "the call failed": the caller decides how to present a failed task and which exit code
// to use, so WaitForTask must hand it back as a normal result.
func TestWaitForTaskReturnsFailedTaskWithoutError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"task_id":"task_1","status":"failed","error":{"message":"生成失败","code":"generation_failed"}}`)
	})
	client := newTestClient(t, mux)

	task, err := client.WaitForTask(context.Background(), "task_1", WaitOptions{Interval: time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatalf("WaitForTask: %v", err)
	}
	if task.Status != StatusFailed || task.Error == nil || task.Error.Code != CodeImageTaskFailed {
		t.Fatalf("task = %+v, want a failed task carrying generation_failed", task)
	}
}

func TestWaitForTaskTimesOut(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, statusSequence(&calls, StatusInProgress))

	task, err := client.WaitForTask(context.Background(), "task_1", WaitOptions{
		Interval: 5 * time.Millisecond,
		Timeout:  300 * time.Millisecond,
	})
	if task != nil {
		t.Errorf("task = %+v, want nil on timeout", task)
	}
	var timeoutErr *WaitTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v (%T), want *WaitTimeoutError", err, err)
	}
	if timeoutErr.TaskID != "task_1" {
		t.Errorf("TaskID = %q, want task_1", timeoutErr.TaskID)
	}
	if timeoutErr.LastStatus != StatusInProgress {
		t.Errorf("LastStatus = %q, want %q", timeoutErr.LastStatus, StatusInProgress)
	}
	// 超时不代表任务结束，文案必须说清楚它还在跑，否则用户会以为额度白花了。
	msg := timeoutErr.Error()
	for _, want := range []string{"task_1", "still running", StatusInProgress} {
		if !strings.Contains(msg, want) {
			t.Errorf("WaitTimeoutError message %q is missing %q", msg, want)
		}
	}
}

// TestWaitForTaskKeepsPollingUnknownStatus guards forward compatibility: a status this
// build has never seen must not be mistaken for success or failure.
func TestWaitForTaskKeepsPollingUnknownStatus(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, statusSequence(&calls, "materialising"))

	_, err := client.WaitForTask(context.Background(), "task_1", WaitOptions{
		Interval: 5 * time.Millisecond,
		Timeout:  200 * time.Millisecond,
	})
	var timeoutErr *WaitTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v (%T), want *WaitTimeoutError", err, err)
	}
	if timeoutErr.LastStatus != "materialising" {
		t.Errorf("LastStatus = %q, want the unknown status to be reported verbatim", timeoutErr.LastStatus)
	}
	if calls.Load() < 2 {
		t.Errorf("polls = %d, want the unknown status to keep the poller going", calls.Load())
	}
}

func TestWaitForTaskPropagatesCallerCancellation(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, statusSequence(&calls, StatusInProgress))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := client.WaitForTask(ctx, "task_1", WaitOptions{Interval: 20 * time.Millisecond, Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v (%T), want context.Canceled rather than a wait timeout", err, err)
	}
}

func TestNextInterval(t *testing.T) {
	tests := []struct {
		current time.Duration
		max     time.Duration
		want    time.Duration
	}{
		{current: 2 * time.Second, max: 15 * time.Second, want: 3 * time.Second},
		{current: 3 * time.Second, max: 15 * time.Second, want: 4500 * time.Millisecond},
		{current: 12 * time.Second, max: 15 * time.Second, want: 15 * time.Second},
		{current: 15 * time.Second, max: 15 * time.Second, want: 15 * time.Second},
	}
	for _, test := range tests {
		if got := nextInterval(test.current, test.max); got != test.want {
			t.Errorf("nextInterval(%s, %s) = %s, want %s", test.current, test.max, got, test.want)
		}
	}
}
