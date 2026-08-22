package profile

import (
	"testing"
)

func TestParseTop(t *testing.T) {
	report := `Showing nodes accounting for 90ms, 90% of 100ms total
      flat  flat%   sum%        cum   cum%
     50ms 50.00% 50.00%      70ms 70.00%  example.com/parser.Decode
     40ms 40.00% 90.00%      40ms 40.00%  runtime.mallocgc
`
	summary := parseTop(report)
	if summary.Total != "100ms total" {
		t.Fatalf("total = %q", summary.Total)
	}
	if len(summary.Functions) != 2 {
		t.Fatalf("function count = %d", len(summary.Functions))
	}
	if summary.Functions[0].Name != "example.com/parser.Decode" {
		t.Fatalf("name = %q", summary.Functions[0].Name)
	}
	if summary.Functions[0].CumulativePercent != 70 {
		t.Fatalf("cumulative percent = %v", summary.Functions[0].CumulativePercent)
	}
}
