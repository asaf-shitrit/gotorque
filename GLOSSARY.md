# Glossary

Domain terms with a fixed meaning in gotorque's code, docs and ADRs. Add a term when a module is named after it.

- **Campaign**: one bounded run of cycles against one target repository at one base revision.
- **Cycle**: one pass of discovery, analysis, optimizer, evaluation, review and policy, ending at the termination check.
- **Attempt**: the 1-based number of a candidate within its campaign. It never repeats, across resumes included.
- **Candidate record**: the persisted verdict for one candidate (`CandidateRecord`). The campaign's candidate history is the list of these records.
- **Ledger**: a campaign's candidate history in attempt order: the recorded candidates handed to the graph, then those it judges. It is the one source of the tallies and the tried targets (`internal/orchestrator/ledger.go`).
- **Tallies**: candidates tried and the two streaks (consecutive failures, consecutive inconclusive). They are always derived from the ledger.
- **Unmeasured candidate**: a candidate rejected before measurement (its patch was invalid, did not apply, failed the shape check, or did not build). Its target gets one more attempt (ADR 0017).
- **Ending**: how a campaign stops: the stop reason, and the failure when a role could not answer. `ended` decides it at three checkpoints (`internal/orchestrator/termination.go`).
- **Sampler**: the port that runs a target under the OS sampling tool and returns a transcript, without judging it (`internal/profile`, ADR 0037).
- **Transcript**: what one sampler run produced (report, sampler exit and output, whether the target was alive at attach), recorded as is.
- **Classification**: `Classify(Transcript)`, the pure mapping of a transcript to a sample or a typed failure (exited early, no frames, idle, unavailable, failed).
- **Idle sample**: a sample whose frames all come from outside the program and its dependencies: the target was caught waiting. It is retried like an empty one.
- **Ladder**: discovery's ordered fallbacks for turning a seed into a sample: same size, larger input, line-repeated variant, next stress seed, then benchmarks (`internal/discovery`).
