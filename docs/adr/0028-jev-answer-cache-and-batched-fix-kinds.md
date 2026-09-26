# 0028. Jev answers are cached per campaign, and fix-kind questions are batched into the cause request

- Status: accepted
- Date: 2026-09-26

## Context

`--analyst jev` re-runs every cycle of the graph loop
(`route_campaign` -> `coordinator` -> `explorer` -> `run_discovery` -> `analyst`)
against the same base revision, and until a candidate is accepted that means
the same hot functions asked about again with the same source. Recorded
campaigns show the cost: go-jsonnet made 64 analyst requests for 4 optimizer
calls (16 a cycle), and dasel made 42 for 3. `--explorer jev` repeats too: its
state is the target's command and `--help` text, neither of which changes
between cycles at a fixed base revision. None of this buys new information; it
buys time, exposure to HTTP 429 (the gateway throttled 36% of attempts at
twelve requests in flight even before this repetition), and a chance for an
answer near a threshold, or a site whose 429 backoff runs out, to reshuffle
targets mid-campaign for no reason but which cycle asked first.

Separately, the fix-kind questions (ADR 0019) went in a second request over
the same state as the cause questions, sent only for a site whose flagged
cause has kinds (allocation or string building). TypeSafe's docs state that an
answer is independent of the other questions asked alongside it ("You can add
or remove questions without changing the others' results"), and gotorque's own
question-count study found the same thing empirically (mean |Δp| 0.004–0.011
across the whole set, kind-baseline recheck within 0.003 at 10 questions). If
that holds, the fix-kind questions can go in the same request as the cause
questions unconditionally, and code can read the ones it needs from the
response it already has, cutting a site with a kind-bearing flagged cause from
two requests to one.

## Decision

**Cache.** `internal/campaign/jevcache.go` wraps `roleSet.CauseEvaluator` and
`roleSet.ExploreEvaluator` in a `cachingEvaluator` at the point both are
attached to the engine (`noteAnalyst`, called from `attachADK` on a fresh
campaign and `SetADK` on resume). The cache key is `jev.Digest(req)`: a SHA-256
of the model id, state, and question set, the exact three things a request
sends. A hit returns the stored `jev.Response` without a network call; a miss
asks the wrapped evaluator and stores the result; an error or partial response
is never stored, so the next identical request gets a real retry rather than a
frozen failure. The cache lives in the campaign's bbolt store
(`Store.JevCacheGet`/`JevCachePut`, bucket `jev_cache`), not in engine memory,
so `--resume` reuses it — anything that must survive resume goes in bbolt,
because in-graph `CampaignState` is rebuilt on every entry. Hit/miss counts
are kept per role in `State.JevCache`, persisted the same way `TokenUsage` is
(riding along on the next `saveEvent`), and rendered in a "Jev cache" table in
the report.

The reviewer (`--reviewer jev`) is not wrapped. Its state carries the
candidate's own patch text and hypothesis, which differ by construction from
one candidate to the next; it was checked against the recorded campaigns and
found not to repeat, so caching it would only ever miss.

**Batching.** `combinedQuestions()` in `internal/campaign/causes.go` merges
`jev.Questions()` (7 cause questions) and `jev.KindQuestions()` (9 fix-kind
questions) into one map — their ids never collide (`alloc`, `string_build`,
... vs. `sb_builder`, `al_size_hint`, ...) — and `causeAnalyst.classify` sends
it in a single request per site. `chooseKinds` no longer makes a request: it
reads the fix-kind answers `classify` already collected, exactly as before,
only for a flagged cause that has kinds. This is a straight code
simplification, not a new answer-reading rule: the decision of which sites get
a kind read, and which kind clears `KindGate`, is unchanged.

The baselines are unaffected by construction: `internal/jev/baseline.go` and
`fixkinds.go` still hash `Questions()` and `KindQuestions()` separately
(`TestBaselineMatchesQuestions`, and the fix-kind digest test), and neither
question's text or state template changed, so both digests, and the values
they guard, stand as measured.

**Validation.** A live replay (`jev-batch-replay.md`) asked 20 functions,
sampled evenly across all seven cause categories from the 186-function
benchmark, twice each: once the old way (a cause request, then a fix-kind
request, both unconditional so every question got both a solo and a batched
answer), once batched. 63 requests total, about $0.003. Result: mean |Δp| =
0.0107 across all 320 answer-pairs, max 0.11 on one function's single
question — in the same range the existing question-count study already
reported. Using raw probability >= 0.5 as a flag proxy, 8 of 320 pairs crossed
it, every one within ±0.06 of exactly 0.5 (a coin flip at its own noise floor,
not a shift caused by batching), and none changed which cause was flagged,
which fix kind was chosen, or how targets would order: `KindGate`'s 0.25 sd
pairwise gap is far larger than anything observed.

## Consequences

- A campaign whose hot functions and base revision do not change between
  cycles pays for Jev's classification once, not once per cycle: the 64
  analyst requests recorded on go-jsonnet's 4-cycle run become the same 12 (or
  fewer, after the fix-kind merge removes the separate kind request) the first
  cycle already made, then zero on every cycle after until something actually
  changes the state — a different hot function, a different revision, or a
  different question set.
- A site with a kind-bearing flagged cause costs one request instead of two,
  independent of caching.
- The cache is only ever a memo of what an identical request already
  answered; it does not change what gets asked, what gets flagged, or what
  the baselines mean. A campaign run without the cache (or with it flushed by
  deleting the campaign directory) gets the same answers, just more slowly.
- Digest-keying by the full question set (not by role or by a fixed id) means
  the analyst's combined request and, hypothetically, any other Jev caller
  asking the identical state and questions would share a cache entry. Today
  only the analyst and explorer are wrapped, so this has no visible effect,
  but it is why `jev.Digest` takes a `Request`, not a role name.

## Alternatives considered

- **Cache only within a single `AnalyzeCauses` call** (not across cycles):
  would not have addressed the actual waste, which is the same call repeating
  across cycles, not within one.
- **Send the fix-kind questions unconditionally as their own request, drop
  only the never-needed ones**: still two round trips per site; the replay
  shows nothing is lost by merging into one.
- **TTL or size-bounded cache**: unnecessary — a campaign's total distinct
  (state, question-set) pairs is bounded by its hot-function cap
  (`maxCauseSites`) and its cycle count, small enough that bbolt's per-campaign
  file was never a concern.
