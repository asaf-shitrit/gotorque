//go:build !darwin && !linux

package campaign

func loadAverage() (float64, bool) { return 0, false }
