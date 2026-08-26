package excerptdebug

import (
	"testing"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/campaign"
)

func TestDebugExtract(t *testing.T) {
	hot := agents.AnalystResult{
		HotPaths: []agents.HotPath{{Location: "cmd/dedupe/main.go:12", Impact: 1, Confidence: 1}},
	}
	excerpts, err := campaign.DebugExtractExcerpts("/Users/factifylaptop3/Desktop/gotorque-targets/dedupe", hot.HotPaths, 32*1024)
	t.Logf("excerpts=%d err=%v", len(excerpts), err)
	for _, e := range excerpts {
		t.Logf("  %s:%d (%d bytes)", e.Path, e.StartLine, len(e.Content))
	}
}
