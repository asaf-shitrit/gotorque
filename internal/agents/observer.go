package agents

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// CallInfo describes one completed attempt at a role's model call.
type CallInfo struct {
	Role     string
	Attempt  int // 1-based
	Duration time.Duration
	Err      error
	// Retrying reports that fence.go will try again, either because the
	// attempt errored or because it returned text that was not valid JSON.
	Retrying bool
}

// CallObserver receives one CallInfo per attempt. Implementations must be safe
// for concurrent use.
type CallObserver func(CallInfo)

// LogCalls returns an observer that writes one line per attempt.
//
// Without it a campaign prints nothing between starting the workflow and the
// first role that happens to log, so a slow call, a silent retry, and a hung
// endpoint are indistinguishable: all three look like a stalled run until the
// agent deadline expires. Diagnosing that difference otherwise costs a whole
// campaign per guess.
func LogCalls(w io.Writer) CallObserver {
	if w == nil {
		return nil
	}
	var mu sync.Mutex
	return func(info CallInfo) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case info.Err != nil:
			_, _ = fmt.Fprintf(w, "[model_call] %s attempt %d failed after %s: %v\n", info.Role, info.Attempt, info.Duration.Round(time.Millisecond), info.Err)
		case info.Retrying:
			_, _ = fmt.Fprintf(w, "[model_call] %s attempt %d returned unparseable output after %s; retrying\n", info.Role, info.Attempt, info.Duration.Round(time.Millisecond))
		default:
			_, _ = fmt.Fprintf(w, "[model_call] %s attempt %d completed in %s\n", info.Role, info.Attempt, info.Duration.Round(time.Millisecond))
		}
	}
}

func (o CallObserver) observe(info CallInfo) {
	if o == nil {
		return
	}
	o(info)
}
