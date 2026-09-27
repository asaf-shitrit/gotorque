package campaign

import (
	"runtime"
	"strconv"
	"strings"
)

// A measurement taken while other work loads the machine can mislead in
// either direction. Overnight on 2026-09-28, a recurring browser test job on
// the same Mac pushed the one-minute load average to 20-25 during several
// candidates' A/B runs, and nothing in their records said so: a contended
// +4.6% read exactly like a clean one. The engine now samples the load
// average before and after a candidate's measurement and records both; the
// report flags the candidate when either exceeds half the machine's CPUs.
// It is evidence for the reader only. The verdict does not read it.

// contendedLoadFraction is the share of runtime.NumCPU above which a load
// average marks the measurement as contended.
const contendedLoadFraction = 0.5

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
