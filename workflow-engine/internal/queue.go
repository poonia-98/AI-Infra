package internal

import (
	"context"
	"errors"
	"sync"
	"time"
)

type ExecutionJob struct {
	ExecutionID string                 `json:"execution_id"`
	WorkflowID  string                 `json:"workflow_id"`
	TriggerType string                 `json:"trigger_type"`
	Input       map[string]interface{} `json:"input"`
	QueuedAt    time.Time              `json:"queued_at"`
}

type ExecutionQueue struct {
	ch     chan ExecutionJob
	closed bool
	mu     sync.RWMutex
}

func NewExecutionQueue(size int) *ExecutionQueue {
	if size <= 0 {
		size = 128
	}
	return &ExecutionQueue{
		ch: make(chan ExecutionJob, size),
	}
}

func (q *ExecutionQueue) Enqueue(ctx context.Context, job ExecutionJob) error {
	q.mu.RLock()
	closed := q.closed
	q.mu.RUnlock()
	if closed {
		return errors.New("queue closed")
	}
	if job.QueuedAt.IsZero() {
		job.QueuedAt = time.Now().UTC()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case q.ch <- job:
		return nil
	}
}

func (q *ExecutionQueue) Dequeue(ctx context.Context) (ExecutionJob, error) {
	select {
	case <-ctx.Done():
		return ExecutionJob{}, ctx.Err()
	case job, ok := <-q.ch:
		if !ok {
			return ExecutionJob{}, errors.New("queue closed")
		}
		return job, nil
	}
}

func (q *ExecutionQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
}

func (q *ExecutionQueue) Len() int {
	return len(q.ch)
}
