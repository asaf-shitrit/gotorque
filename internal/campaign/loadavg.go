package campaign

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// A measurement taken while other work loads the machine can mislead in
// either direction. Overnight on 2026-09-28, a recurring browser test job on
// the same Mac pushed the one-minute load average to 20-25 during several
// candidates' A/B runs, and nothing in their records said so: a contended
// +4.6% read exactly like a clean one. The engine now samples the load
// average before and after a candidate's measurement and records both; the
// report flags the candidate when either exceeds contendedLoadFraction per CPU.
// It is evidence for the reader only. The verdict does not read it.

// contendedLoadFraction is the share of runtime.NumCPU above which a load
// average marks the measurement as contended. It was 0.5 at first, but this
// 10-CPU Mac idles at a load of 4.4-4.9 from its other sessions, so the quiet
// wait spent 140-170 s per candidate waiting out ordinary background work
// (overnight-dasel-history-2). At 0.7 it still catches every contended run
// seen overnight: miller-1's false accept measured at 7.45-11.67, and the
// browser job's bursts at 7.8-25.
const contendedLoadFraction = 0.7

// sampleLoad returns the current one-minute load average, or nil when the
// platform does not expose one.
func sampleLoad() []float64 {
	if l, ok := loadAverage(); ok {
		return []float64{l}
	}
	return nil
}

// contended reports whether any sampled load exceeds the threshold for a
// machine with cpus CPUs.
func contended(loads []float64, cpus int) bool {
	limit := contendedLoadFraction * float64(cpus)
	for _, l := range loads {
		if l > limit {
			return true
		}
	}
	return false
}

func machineCPUs() int { return runtime.NumCPU() }

func fixedLoad(value uint32, scale uint64) (float64, bool) {
	if scale == 0 {
		return 0, false
	}
	return float64(value) / float64(scale), true
}

func parseProcLoadavg(text string) (float64, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0, false
	}
	l, err := strconv.ParseFloat(fields[0], 64)
	return l, err == nil
}

// Recording contention after the fact only tells the reader which verdicts to
// doubt. Before a candidate is measured, the engine therefore waits, for at
// most quietWaitLimit, for the load average to fall back under the contended
// threshold: the overnight browser job ran in bursts of a few minutes, so a
// short wait moves most measurements out of it. A wait that runs out does not
// block the candidate; it is measured anyway and flagged as before.
const (
	quietWaitLimit = 3 * time.Minute
	quietPoll      = 10 * time.Second
)

// quietWaiter holds what waitForQuiet needs, so a test can drive it without
// sleeping or reading the real load average.
type quietWaiter struct {
	sample func() []float64
	cpus   int
	limit  time.Duration
	poll   time.Duration
	sleep  func(context.Context, time.Duration) error
}

func defaultQuietWaiter() quietWaiter {
	return quietWaiter{sample: sampleLoad, cpus: machineCPUs(), limit: quietWaitLimit, poll: quietPoll, sleep: sleepContext}
}

// wait returns how long it waited and whether the machine was still
// contended when it stopped.
func (w quietWaiter) wait(ctx context.Context) (time.Duration, bool) {
	var waited time.Duration
	for contended(w.sample(), w.cpus) {
		if waited >= w.limit || w.sleep(ctx, w.poll) != nil {
			return waited, true
		}
		waited += w.poll
	}
	return waited, false
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
