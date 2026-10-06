package campaign

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
