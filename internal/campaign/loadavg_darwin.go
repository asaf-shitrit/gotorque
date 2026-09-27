package campaign

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// loadAverage reads the one-minute load average from the kernel's
// vm.loadavg (struct loadavg: three fixed-point uint32 averages, then the
// fixed-point scale as a long).
func loadAverage() (float64, bool) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 24 {
		return 0, false
	}
	scale := binary.LittleEndian.Uint64(raw[16:24])
	return fixedLoad(binary.LittleEndian.Uint32(raw[0:4]), scale)
}
