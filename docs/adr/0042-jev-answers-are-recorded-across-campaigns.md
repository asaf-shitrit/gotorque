# 0042. Jev answers are recorded across campaigns, keyed by the exact question asked

- Status: accepted
- Date: 2026-10-25

## Context

Jev decides which functions a campaign tries: the analyst asks seven yes/no
cause questions about each hot function, ranks the answers against Jev's usual
answer, and flags a cause that clears both gates — a z-score at or above
`FlagThreshold` (0.5) and a probability at or above `ProbabilityFloor` (0.5)
(ADR 0025). Jev does not answer the same question about the same source
identically twice, and both gates sit inside that noise.

Measured on the record: the 156 campaign databases under
`~/projects/gotorque-work/campaigns` hold 4247 site classifications over 444
distinct functions. Grouping by (target, file, function, cause) leaves 3108
keys, of which 3080 were asked in two or more campaigns. 1415 of those moved
between campaigns; the widest move for one key was 0.130. Thirty-five cause
flags flipped, 26 of them on development targets and 9 on held-out ones; 21
flipped across the 0.5 probability floor and 14 across the z threshold. At the
function level, 13 of the 440 functions classified in two or more campaigns
lost every flag in some campaign and held one in another. The issue's own
evidence is the same shape: gofumpt's sweep 5 found nothing to flag on the
identical discovery that flagged a target in sweep 4.

The answer cache (ADR 0028) removes this repetition within one campaign. It
does nothing across campaigns, which is where the trials that make up a
stability claim live: each campaign starts with an empty cache and asks again.

## Decision

**Record every answered request, and answer from the record.** A new
`campaign.AnswerRecord` (internal/campaign/jevanswers.go) is a bbolt file at
`<user cache>/gotorque/jev-answers.db` holding one entry per answered request,
keyed by `jev.Digest(req)` — the pinned build, the state (the function's file
and whole source), and the question set. `cachingEvaluator` consults it after
its own campaign cache and before Jev: a hit costs no request and is counted as
`Shared` in the report; a miss asks Jev and writes the answer to both.

The key is content, not campaign: two campaigns on the same function at the
same source text get the same answers, and a campaign on a different target
containing byte-identical source gets them too. That is the property the issue
asks for — the same function keeps its verdict until its code changes.

Three things make it safe to have on by default:

- **The build is in the key.** `cachingEvaluator` fills `req.Model` with
  `jev.Model` before digesting. The live client fills it anyway, so nothing
  changes on the wire, but without it an upgrade that moves the pinned build
  (ADR 0029) would replay answers measured against the build it replaced.
- **Only a live campaign records.** `--adk-stub` runs `jev.Stub`, and its
  answers would enter the record under the real build's key; the CLI attaches
  the record only when `--adk` is given.
- **A record is never required.** It cannot be opened (another gotorque
  process holds it, the cache directory is not writable), it cannot be read,
  or it cannot be written: the campaign warns and asks Jev, exactly as a
  failed cache write does today. `--jev-answers off` asks Jev in every
  campaign, `--jev-answers <path>` uses another file.

The record is content-addressed and never pruned. It grows by one small entry
per distinct request ever answered, so deleting it is always safe: the next
campaign asks again, and its answers become the new record.

## Evidence

The measurement above is a replay, not a new campaign: the record's 1415 moved
answers and 35 flag flips are what the recorded campaigns already show, and the
record removes them by construction — the second campaign never asks. It also
removes the waste: 3108 distinct requests stand behind the 4247 classifications
in that record, so about 89% of those analyst requests would have been served
without a call.

`TestAnalystFlagsStayStableAcrossCampaignsOnTheSameSource` pins the failure and
the fix through the real analyst: the same fixture, two campaigns, an answer
that drifts from 0.51 to 0.49 across the floor. Without the record the second
campaign flags nothing; with it, it asks nothing and flags the same target.
`TestAFlagOutsideTheRecordStillDrifts` keeps that test honest by running the
same drift with no record and asserting the flip is still there.

Nothing here touches the baselines: `internal/jev/baseline.go` and
`fixkinds.go` hash the question text and state template, neither of which
changed, and `TestBaselineMatchesQuestions` still passes. No live campaign was
run — the OpenRouter account behind `OPENROUTER_API_KEY` is out of credits
(HTTP 402 at preflight), so the cost and the answer distribution of a live
campaign against the record stay unmeasured.

## Consequences

- A campaign's target set no longer depends on which campaign asked first: the
  answer is a function of the source text. A second campaign on the same
  revision reproduces the first one's flags exactly, and the report says how
  many answers came from the record.
- The report gains a `Shared` column in its Jev cache table and a header line
  naming the record path, so a reader can tell a campaign that asked Jev from
  one that replayed earlier answers. A resume does not carry
  `--jev-answers` with it, so it names the record again; a resume that turns
  the record off clears the path the earlier run persisted, and the header
  then claims nothing.
- Reproducibility is per record, not per build: a machine that has never run a
  campaign on this source still gets whatever Jev answers the first time. The
  record is a file, so it can be copied or kept with the campaigns that judge
  a revision.
- An answer Jev gives once is the answer for that source until the source, the
  question set, or the pinned build changes. A single unlucky answer no longer
  costs one campaign but every campaign on that source. That is the trade the
  issue asks for, and it is visible rather than silent because the counters
  separate reused answers from fresh ones.
- The record is one more file with an exclusive lock. Two campaigns that run
  at the same time on one machine will have one of them run without the
  record; that campaign still works, it just records the cost.

## Alternatives considered

- **Ask each near-threshold question k times and take the majority.** The
  issue's first option. It needs a live measurement to pick k and to show the
  cost, and the account is out of credits; it also spends more per campaign
  than this option, which spends less. It remains sensible on top of this one
  for a source the record has never seen.
- **Record only the flag margin in the report.** The issue's third option, and
  the least it would accept. The site verdicts already persist every score, so
  a margin is derivable today; recording it makes an inconsistency readable but
  leaves it.
- **Write answers into the campaign directory and read them back explicitly
  with `--history`.** `--history` exists for tried targets (ADR 0031), but
  making reproducibility depend on an operator passing the right earlier
  campaign is a rule nobody would remember. A content-addressed file is the
  same guarantee without the ceremony.
- **Key by the function's location line instead of its source.** Line numbers
  move with unrelated edits; the source text does not. This is the reasoning
  ADR 0022 already uses to match a function by name rather than by line.
- **Per-question keys, so a change to the question set keeps the answers to
  the untouched questions.** ADR 0028 established that an answer does not
  depend on the questions asked beside it, so this would be sound. It buys
  reuse only when the question set itself changes (a new cause question ships),
  which also invalidates the baseline that the answers are ranked against
  (`internal/jev/baseline.go`): those answers would have to be re-measured
  anyway. Request-level keys keep the digest identical to the campaign cache's.
