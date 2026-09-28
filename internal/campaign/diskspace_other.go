//go:build !darwin && !linux

package campaign

func freeBytes(string) (uint64, bool) { return 0, false }
