package discovery

import (
	"context"
	"slices"

	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// unbufferedWriteShare is the fraction of a function's sampled time that must
// be unbuffered writes before code raises the target itself. Across every
// campaign sample on record, the functions above it were exactly gron's
// output loop, gojq's printValues and encoder flush, and fzf's Printer, each
// at 0.95 or more; nothing else reached 0.3.
const unbufferedWriteShare = 0.5

// maxWriteCallers caps how many callers a write site's evidence keeps.
const maxWriteCallers = 3

// unbufferedWrites resolves the sample's unbuffered-write sites to hot-list
// locations, keeping each function's largest share over the sampled
// workloads. A site outside the hot list is dropped: the evidence ranks
// targets and is not a reason to look at code discovery did not measure. The
// hot list is a parameter, not read from a shared field, so it cannot be asked
// before it exists.
func unbufferedWrites(ctx context.Context, loc locator, hot []string, own func(string) bool, results []profile.SampleResult) []orchestrator.UnbufferedWrite {
	best := map[string]profile.WriteSite{}
	var order []string
	for _, result := range results {
		for _, site := range profile.UnbufferedWrites(result.Stacks, own, unbufferedWriteShare) {
			prior, seen := best[site.Function]
			if !seen {
				order = append(order, site.Function)
			}
			if !seen || site.Share > prior.Share {
				best[site.Function] = site
			}
		}
	}
	var out []orchestrator.UnbufferedWrite
	for _, name := range order {
		if w, ok := writeEvidence(ctx, loc, hot, best[name]); ok {
			out = append(out, w)
		}
	}
	return out
}

func writeEvidence(ctx context.Context, loc locator, hot []string, site profile.WriteSite) (orchestrator.UnbufferedWrite, bool) {
	where := loc.location(ctx, "", site.Function)
	if !slices.Contains(hot, where) {
		return orchestrator.UnbufferedWrite{}, false
	}
	w := orchestrator.UnbufferedWrite{Location: where, Share: site.Share}
	for _, caller := range site.Callers {
		if len(w.Callers) == maxWriteCallers {
			break
		}
		if c := loc.location(ctx, "", caller); c != caller && c != where && !slices.Contains(w.Callers, c) {
			w.Callers = append(w.Callers, c)
		}
	}
	return w, true
}
