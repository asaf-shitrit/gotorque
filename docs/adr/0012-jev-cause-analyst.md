# 0012. Jev cause classification may replace the analyst model

- Status: accepted (opt-in, `--analyst jev`)
- Date: 2026-09-22

## Context

The analyst role turns discovery's hot functions into likely causes and hypotheses for the
optimizer. As a model role it is slow (role calls of one to four minutes), reformats the `path:line`
locations the excerpt collector depends on, and on gron sent the first two candidates of every
campaign at rune classification because the benchmark profile it was shown was dominated by unicode
lookups.

TypeSafe's Jev, served by the Vercel AI Gateway, is a classifier rather than a text model: it
answers typed questions about a state with calibrated probabilities. A benchmark of 93 real
single-function performance fixes (golang/go history plus unbuffered-IO fixes mined from GitHub, the
cause taken from the fixing commit's subject), each asked about before and after the fix, measured
it against the incumbent analyst model (`deepseek/deepseek-v4.1-flash`) and a regex baseline:

| | Jev | LLM | regex |
|---|---|---|---|
| Probability higher on the unfixed code than the fixed (81 cases) | 0.88 [0.82, 0.93] | 0.79 [0.72, 0.85] | 0.62 |
| Same, drop of at least 0.1 | 0.59 | 0.64 | 0.31 |
| Same, with a misleading profile in the state (63 cases) | 0.89 | 0.73 | 0.63 |
| Names the cause, answers normalized per question | 0.51 (93 cases) | 0.59 (81 cases) | 0.31 |
| Median / p95 latency per function | 0.5 s / 1.0 s | 89 s / 21 min | — |
| Cost per function | $0.00007 | $0.006 | — |
| Calls that failed after retries | 0 | 9% clean, 26% with the profile | — |

Brackets are 95% bootstrap intervals, stratified by cause. Jev is as accurate as the model at this
task, more robust to misleading context, two orders of magnitude faster and cheaper, and nearly
deterministic (repeat calls moved answers by 0.01 on average). Unbuffered IO, the cause behind the
only accepted gron patch, is its strongest class: 8 of 8 fixes drop sharply.

## Decision

Offer `--analyst jev`, which swaps the analyst agent node for a deterministic node of the same name
(`orchestrator.Dependencies.Causes`, implemented in `internal/campaign/causes.go`). For each
discovery hot function with a source position it sends the whole function and seven yes/no
questions, one per cause, and ranks the answers in code against Jev's measured usual answer to each
question (`internal/jev/baseline.go`), flagging causes at least half a standard deviation above it,
two at most. The state never carries the profile.

## Consequences

The analysis costs seconds instead of minutes, keeps discovery's locations verbatim, and records
per-function scores in a `cause_analysis` event. Its output is still advice: it feeds the optimizer
and never reaches `apply_policy`. The baseline is tied to the exact question text and state
template (`TestBaselineMatchesQuestions`); rewording either means re-measuring with
`TestLiveBaseline`. A second provider and key (`AI_GATEWAY_API_KEY`) enter the run, guarded by a
preflight under `--adk` because a gateway account without billing refuses every request.

The baseline cannot be pinned to a model version. TypeSafe advises pinning once thresholds are
tuned, but the Vercel gateway serves only the alias `typesafe-ai/jev` and reports no version, so a
Jev release (1.13 at measurement) silently moves every answer the baseline describes. Re-measure
on each release; switching to TypeSafe's own endpoint, which accepts pinned IDs, needs a second
account and key.

Jev only classifies what discovery lists. On gron the output loop behind the accepted `bufio` patch
shows up only as `fmt.(*pp).doPrintln`, a standard-library frame discovery drops, so the analyst is
never asked about it (asked directly, it flags unbuffered IO at +3.3 sd). It is weakest on missing
fast paths and repeated work, and cannot estimate how large a win would be (rank correlation with
measured speedups 0.06).

## Alternatives considered

- One "which cause" choice question: the weakest way to name a cause (0.32) and the only format a
  misleading profile derailed (0.16).
- One Score per cause on a shared four-level scale: raw scores became comparable (0.48), but no
  better than normalized yes/no answers, and fixed code kept scoring high (the before/after drop
  rate fell from 0.52 to 0.38).
- Splitting the three compound questions into one fact each, combined in code as a product, as the
  Noul guidance suggests for independent conditions: naming the cause did not change (0.50), but
  fix detection fell from 0.49 to 0.38 [−0.18, −0.05] and the cause stayed flagged on more fixed
  code (+0.11). Each part describes a fact that survives the fix (a function still appends after
  gaining a capacity hint, and its size becomes more obviously known), so only the relationship
  separates broken from fixed code — what the TypeSafe skill means by splitting "without destroying
  the relationship being judged".
- Structured criteria with a definition and examples on both sides: no net gain (string-building fix
  detection fell from 0.54 to 0.38 while IO and superlinear answers sharpened). Naming the state
  field (`` `source` ``) in every question measured neutral and was adopted, since the guidance
  asks for it and Jev reads literally.
- Weights fitted on the benchmark's labels: 0.55 against 0.51, inside the noise of 93 cases over
  seven causes. Revisit once campaign verdicts supply labels.
- Gating candidates on Jev's predicted impact before measuring them: rejected. It would let a model
  output end a candidate, which the deterministic policy owns, and the impact prediction carried no
  signal anyway.
- Showing Jev the profile: rejected; it pulled every answer toward whatever the profile was
  dominated by.
