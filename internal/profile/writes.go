package profile

import (
	"cmp"
	"slices"
	"strings"
)

// WriteSite is an own function whose sampled time is mostly write system
// calls made with no bufio frame between it and the kernel: every record it
// emits is its own write. Share is that time as a fraction of all the time
// attributed to the function; Callers are the own frames that called it on
// those paths, most samples first, where a loop over records usually lives.
type WriteSite struct {
	Function string
	Share    float64
	Callers  []string
}

type writeTally struct {
	total, unbuffered, direct float64
	callers                   map[string]float64
}

// UnbufferedWrites finds the own functions whose attributed samples are at
// least minShare unbuffered writes. It attributes samples exactly as
// AttributeToOwn does, orphaned write samples included: those are shared
// among the functions seen calling syscall.write in proportion to how often
// each was, and count as unbuffered in the proportion each function's
// observed writes were. On fzf's filter mode the Printer closure,
// func(str string) { fmt.Println(str) }, carried the second-largest weight of
// the sample, almost all of it here, while Jev, shown the 2.4 KB options
// constructor the closure sits in, saw no unbuffered I/O at all.
func UnbufferedWrites(stacks []Stack, own func(string) bool, minShare float64) []WriteSite {
	tallies := map[string]*writeTally{}
	var orphaned float64
	for _, s := range stacks {
		i := innermost(s.Frames, own)
		if i < 0 {
			if call, ok := orphanSyscall(s.Frames); ok && call == "write" {
				orphaned += float64(s.Weight)
			}
			continue
		}
		tallyStack(tallies, s, i, own)
	}
	shareOrphanWrites(tallies, orphaned)
	return writeSites(tallies, minShare)
}

func tallyStack(tallies map[string]*writeTally, s Stack, i int, own func(string) bool) {
	t := tallies[s.Frames[i]]
	if t == nil {
		t = &writeTally{callers: map[string]float64{}}
		tallies[s.Frames[i]] = t
	}
	w := float64(s.Weight)
	t.total += w
	if call, ok := syscallBelow(s.Frames[:i]); !ok || call != "write" {
		return
	}
	t.direct += w
	if slices.ContainsFunc(s.Frames[:i], isBufioFrame) {
		return
	}
	t.unbuffered += w
	if j := innermost(s.Frames[i+1:], own); j >= 0 {
		t.callers[s.Frames[i+1+j]] += w
	}
}

func isBufioFrame(frame string) bool { return strings.HasPrefix(frame, "bufio.") }

func shareOrphanWrites(tallies map[string]*writeTally, orphaned float64) {
	var direct float64
	for _, t := range tallies {
		direct += t.direct
	}
	if orphaned == 0 || direct == 0 {
		return
	}
	for _, t := range tallies {
		t.total += orphaned * t.direct / direct
		t.unbuffered += orphaned * t.unbuffered / direct
	}
}

func writeSites(tallies map[string]*writeTally, minShare float64) []WriteSite {
	var sites []WriteSite
	for name, t := range tallies {
		if t.total == 0 || t.unbuffered/t.total < minShare {
			continue
		}
		callers := make([]string, 0, len(t.callers))
		for c := range t.callers {
			callers = append(callers, c)
		}
		slices.SortFunc(callers, func(a, b string) int {
			return cmp.Or(cmp.Compare(t.callers[b], t.callers[a]), cmp.Compare(a, b))
		})
		sites = append(sites, WriteSite{Function: name, Share: t.unbuffered / t.total, Callers: callers})
	}
	slices.SortFunc(sites, func(a, b WriteSite) int {
		return cmp.Or(cmp.Compare(b.Share, a.Share), cmp.Compare(a.Function, b.Function))
	})
	return sites
}
