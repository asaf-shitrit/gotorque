// Package jobs provides a small, deterministic in-memory job manager for
// asynchronous harness operations. It deliberately knows nothing about the
// command runner, storage, or ADK workflow.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"example.com/go-agent-optimizer/internal/domain"
)

var (
	ErrNotFound       = errors.New("job not found")
	ErrNotCancellable = errors.New("job is already terminal")
)

// Result identifies a result persisted by the operation that ran as a job.
// The job manager stores no opaque operation data; consumers retrieve it from
// the resource URI through the appropriate evidence store.
type Result struct {
	ResultURI string
}

// Task is invoked once for a submitted job. The context is cancelled when the
// job is cancelled. Tasks should arrange cleanup before returning.
type Task func(context.Context) (Result, error)

// Manager is the narrow job API required by the MCP control plane.
type Manager interface {
	Submit(kind string, task Task) (domain.Job, error)
	Get(id string) (domain.Job, error)
	Cancel(id string) (domain.Job, error)
}

// Options makes tests and embedders deterministic without imposing a clock or
// ID format on callers.
type Options struct {
	Now    func() time.Time
	NextID func() string
}

// MemoryManager is a concurrency-safe, process-local implementation of
// Manager. Jobs are retained for the lifetime of the manager.
type MemoryManager struct {
	mu     sync.RWMutex
	now    func() time.Time
	nextID func() string
	jobs   map[string]*record
}

type record struct {
	job    domain.Job
	cancel context.CancelFunc
}

// NewMemoryManager creates an in-memory job manager. Default IDs are stable
// within a process (job-000001, job-000002, ...), which keeps logs and tests
// reproducible.
func NewMemoryManager(options Options) *MemoryManager {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	nextID := options.NextID
	if nextID == nil {
		var mu sync.Mutex
		sequence := 0
		nextID = func() string {
			mu.Lock()
			defer mu.Unlock()
			sequence++
			return fmt.Sprintf("job-%06d", sequence)
		}
	}
	return &MemoryManager{now: now, nextID: nextID, jobs: make(map[string]*record)}
}

// Submit records a queued job and starts its task asynchronously.
func (m *MemoryManager) Submit(kind string, task Task) (domain.Job, error) {
	if kind == "" {
		return domain.Job{}, errors.New("job kind is required")
	}
	if task == nil {
		return domain.Job{}, errors.New("job task is required")
	}

	m.mu.Lock()
	id := m.nextID()
	if _, exists := m.jobs[id]; exists {
		m.mu.Unlock()
		return domain.Job{}, fmt.Errorf("duplicate job ID %q", id)
	}
	now := m.now()
	ctx, cancel := context.WithCancel(context.Background())
	record := &record{job: domain.Job{
		ID:        id,
		Kind:      kind,
		Status:    domain.JobQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}, cancel: cancel}
	m.jobs[id] = record
	snapshot := record.job
	m.mu.Unlock()

	go m.run(id, ctx, task)
	return snapshot, nil
}

func (m *MemoryManager) run(id string, ctx context.Context, task Task) {
	m.transitionToRunning(id)
	result, err := runTask(ctx, task)

	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.jobs[id]
	if !ok || record.job.Status == domain.JobCancelled {
		return
	}
	record.job.UpdatedAt = m.now()
	if err != nil {
		record.job.Status = domain.JobFailed
		record.job.Error = err.Error()
		return
	}
	record.job.Status = domain.JobSucceeded
	record.job.ResultURI = result.ResultURI
}

func runTask(ctx context.Context, task Task) (result Result, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("job task panicked: %v", recovered)
		}
	}()
	return task(ctx)
}

func (m *MemoryManager) transitionToRunning(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.jobs[id]
	if !ok || record.job.Status != domain.JobQueued {
		return
	}
	record.job.Status = domain.JobRunning
	record.job.UpdatedAt = m.now()
}

// Get returns a snapshot of a job.
func (m *MemoryManager) Get(id string) (domain.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.jobs[id]
	if !ok {
		return domain.Job{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return record.job, nil
}

// Cancel marks a queued or running job cancelled and signals its task. A task
// that ignores its context cannot be forcibly terminated, but it cannot later
// overwrite the cancelled terminal state.
func (m *MemoryManager) Cancel(id string) (domain.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.jobs[id]
	if !ok {
		return domain.Job{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if isTerminal(record.job.Status) {
		return record.job, fmt.Errorf("%w: %s", ErrNotCancellable, id)
	}
	record.job.Status = domain.JobCancelled
	record.job.UpdatedAt = m.now()
	record.cancel()
	return record.job, nil
}

func isTerminal(status domain.JobStatus) bool {
	return status == domain.JobSucceeded || status == domain.JobFailed || status == domain.JobCancelled
}
