package agents

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLogCallsReportsEveryOutcome(t *testing.T) {
	tests := []struct {
		name string
		info CallInfo
		want []string
	}{
		{
			name: "completed",
			info: CallInfo{Role: "analyst", Attempt: 1, Duration: 12500 * time.Millisecond},
			want: []string{"analyst", "attempt 1", "completed in 12.5s"},
		},
		{
			name: "failed",
			info: CallInfo{Role: "optimizer", Attempt: 2, Duration: 4 * time.Minute, Err: errors.New("context deadline exceeded")},
			want: []string{"optimizer", "attempt 2", "failed after 4m0s", "context deadline exceeded"},
		},
		{
			name: "started",
			info: CallInfo{Role: "reviewer", Attempt: 3, Started: true},
			want: []string{"reviewer", "attempt 3", "started"},
		},
		{
			name: "retrying on unparseable output",
			info: CallInfo{Role: "reviewer", Attempt: 3, Duration: time.Second, Retrying: true},
			want: []string{"reviewer", "attempt 3", "unparseable", "retrying"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sb strings.Builder
			LogCalls(&sb)(tt.info)
			line := sb.String()
			for _, want := range tt.want {
				if !strings.Contains(line, want) {
					t.Errorf("line %q missing %q", line, want)
				}
			}
			if !strings.HasSuffix(line, "\n") {
				t.Errorf("line %q should end with a newline", line)
			}
		})
	}
}

func TestNilObserverIsSafe(t *testing.T) {
	var observer CallObserver
	observer.observe(CallInfo{Role: "analyst"}) // must not panic
	if LogCalls(nil) != nil {
		t.Error("LogCalls(nil) should return a nil observer")
	}
}
