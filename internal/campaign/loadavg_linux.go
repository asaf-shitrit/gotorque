package campaign

import "os"

// loadAverage reads the one-minute load average from /proc/loadavg.
func loadAverage() (float64, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	return parseProcLoadavg(string(data))
}
