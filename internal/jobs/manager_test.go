package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"example.com/go-agent-optimizer/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestMemoryManagerCompletesTask(t *testing.T) {
	clock := fixedClock()
	manager := NewMemoryManager(Options{Now: clock.Now})
	finished := make(chan struct{})

	job, err := manager.Submit("repository-inspect", func(context.Context) (Result, error) {
		close(finished)
		return Result{ResultURI: "repo://r/inventory"}, nil
	})
	require.NoError(t, err)
	require.Equal(t, "job-000001", job.ID)
	<-finished

	completed := waitFor(t, manager, job.ID, domain.JobSucceeded)
	require.Equal(t, "repo://r/inventory", completed.ResultURI)
	require.Empty(t, completed.Error)
	require.True(t, completed.UpdatedAt.After(completed.CreatedAt))
}

func TestMemoryManagerCancellationIsTerminal(t *testing.T) {
	manager := NewMemoryManager(Options{})
	started := make(chan struct{})
	release := make(chan struct{})
	job, err := manager.Submit("workload-run", func(ctx context.Context) (Result, error) {
		close(started)
		<-ctx.Done()
		<-release
		return Result{ResultURI: "run://ignored/summary"}, nil
	})
	require.NoError(t, err)
	<-started

	cancelled, err := manager.Cancel(job.ID)
	require.NoError(t, err)
	require.Equal(t, domain.JobCancelled, cancelled.Status)
	close(release)

	time.Sleep(time.Millisecond)
	stored, err := manager.Get(job.ID)
	require.NoError(t, err)
	require.Equal(t, domain.JobCancelled, stored.Status)
	require.Empty(t, stored.ResultURI)
	_, err = manager.Cancel(job.ID)
	require.ErrorIs(t, err, ErrNotCancellable)
}

func TestMemoryManagerTaskFailureAndMissingJob(t *testing.T) {
	manager := NewMemoryManager(Options{})
	job, err := manager.Submit("evaluation", func(context.Context) (Result, error) {
		return Result{}, errors.New("measurement failed")
	})
	require.NoError(t, err)
	failed := waitFor(t, manager, job.ID, domain.JobFailed)
	require.Equal(t, "measurement failed", failed.Error)
	_, err = manager.Get("missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func waitFor(t *testing.T, manager Manager, id string, expected domain.JobStatus) domain.Job {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, err := manager.Get(id)
		require.NoError(t, err)
		if job.Status == expected {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %s did not reach status %s", id, expected)
	return domain.Job{}
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func fixedClock() *testClock { return &testClock{t: time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Nanosecond)
	return c.t
}
