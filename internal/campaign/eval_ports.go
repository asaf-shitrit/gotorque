package campaign

import (
	"context"
	"time"
)

// The ports of candidate evaluation: the few places where it reaches outside
// itself and where a second implementation is real, because the campaign's
// own is not what a test, a verification or a different host would use.

// journal is where an evaluation reports what it did that a reader of the
// campaign should see: the events its stages emit, and the isolation
// degradations its runs observed. The campaign's adapter is engineJournal,
// which persists synchronously; tests substitute a recording one.
type journal interface {
	event(kind, message string, data any) error
	isolationNotes(notes []string)
}

// engineJournal is the campaign's journal: each event is saved to the
// campaign's store before event returns, and isolation notes join the
// campaign's state.
type engineJournal struct{ e *Engine }

func (j engineJournal) event(kind, message string, data any) error {
	return j.e.saveEvent(kind, message, data)
}

func (j engineJournal) isolationNotes(notes []string) { j.e.recordIsolationNotes(notes) }

// evalJournal is the journal evaluation reports to: the one set on the engine,
// or the campaign's own.
func (e *Engine) evalJournal() journal {
	if e.journal != nil {
		return e.journal
	}
	return engineJournal{e}
}

// machine is the host a measurement runs on, as far as evaluation cares: how
// loaded it is, whether that load is enough to distrust a reading, and how long
// to wait for it to settle. The load average is global to the process, which
// is why this is a port: nothing about a candidate's measurement can otherwise
// be made to meet a loaded machine on demand. hostMachine is the real one.
type machine interface {
	// load samples the machine's current load, nil when it has no reading.
	load() []float64
	// contended reports whether any of the sampled loads marks a measurement
	// as taken on a contended machine.
	contended(loads []float64) bool
	// waitQuiet waits, for a bounded time, for the load to fall, and returns
	// how long it waited and whether the machine was still contended when it
	// stopped.
	waitQuiet(ctx context.Context) (waited time.Duration, expired bool)
}

// hostMachine is the machine the process runs on.
type hostMachine struct{}

func (hostMachine) load() []float64 { return sampleLoad() }

func (hostMachine) contended(loads []float64) bool { return contended(loads, machineCPUs()) }

func (hostMachine) waitQuiet(ctx context.Context) (time.Duration, bool) {
	return defaultQuietWaiter().wait(ctx)
}

// evalMachine is the machine evaluation measures on: the one set on the
// engine, or the host.
func (e *Engine) evalMachine() machine {
	if e.machine != nil {
		return e.machine
	}
	return hostMachine{}
}
