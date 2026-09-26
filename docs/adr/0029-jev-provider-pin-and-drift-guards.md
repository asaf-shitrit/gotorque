# 0029. Jev requests are pinned to TypeSafe's own provider, and Preflight guards against drift

- Status: accepted
- Date: 2026-09-26

## Context

ADR 0012 accepted that the cause baseline (and, by ADR 0014, the review baseline) cannot be pinned
to a Jev model version: the Vercel AI Gateway serves only the unversioned alias `typesafe-ai/jev`,
a pinned ID such as `jev-1.13.0` returns 404, and no response header or field named a version. That
ADR's mitigation was "re-measure `TestLiveBaseline` when TypeSafe ships a release" — a mitigation
that depends on someone noticing a release happened.

A follow-up probe (`jev-model-pinning.md`, three live calls) found a second, previously unknown
source of variance: the gateway serves the same alias from more than one upstream. A plain
`/v1/evaluate` call showed `providerMetadata.gateway.routing` planning `digitalocean -> typesafe-ai`
as its execution order, with `resolvedProvider: "digitalocean"` but `finalProvider: "typesafe-ai"`
after DigitalOcean's leg failed. Nothing confirms DigitalOcean, when it does answer, runs the same
build TypeSafe's own endpoint does. The probe also found the one version signal the gateway does
expose: `GET /typesafe/v1/models` returns a `release_date` per aliased model, free of charge, which
this ADR uses as a first, cheap check.

Neither problem can be fixed by pinning a model ID. Both can be narrowed in code:

1. The AI Gateway's evaluation modality documents `providerOptions.gateway.only`, which restricts
   which upstream may serve a request. An availability measurement (ten live single-attempt calls
   to `/v1/evaluate`, five pinned to `only: ["typesafe-ai"]` and five unpinned, no retries) found
   3/10 successes pinned and 4/5 unpinned, but every failure — pinned or not — carried
   `modelAttemptCount: 1, providerAttemptCount: 0`: the gateway's own per-model rate limit rejected
   the request before it reached any provider, the same throttling `client.go` already documents
   and retries against (36% of attempts at load, a 429 that can outlast a 15 s ladder). Restricting
   the provider set did not change that failure mode; every call that did reach a provider, pinned
   or not, was answered by `typesafe-ai`. Pinning is adopted on that evidence.
2. `GET /typesafe/v1/models` is free (no tokens spent) and returns the alias's `release_date`. The
   baselines record the date they were measured against; a mismatch is worth surfacing before a
   campaign spends on stale-baseline answers.
3. A release can move Jev's answers without moving `release_date` — nothing documents whether the
   date changes on every release behind the alias, only that it existed at all at measurement time.
   A canary — a fixed function, a fixed subset of the cause questions, and recorded answers to
   compare against — catches that case directly, the same way the cause and review baselines
   themselves are the real detector of a drifted model, just spent on every preflight instead of
   only when someone thinks to re-run `TestLiveBaseline`.

## Decision

- Every request `internal/jev.Client.Evaluate` sends sets `providerOptions.gateway.only:
  ["typesafe-ai"]`, unless the caller already set its own `ProviderOptions` (nothing in this
  codebase does). The response's `providerMetadata.gateway.routing.finalProvider` is checked
  against `typesafe-ai`; a different value refuses the answer with an error, not a retry, since
  retrying the same pinned-only request cannot resolve to a different provider. A response that
  carries no routing metadata at all (a test stub, or a gateway shape change) is accepted rather
  than refused: this is a positive check, not a default-deny on absence.
- `baselineModelRelease = "2026-09-15"` sits beside `baselineDigest` in `internal/jev/baseline.go`,
  the date `GET /typesafe/v1/models` reported for `jev` when both baselines were measured;
  `review_baseline.go` references it rather than duplicating it, since both baselines share one
  Jev version. `Client.Preflight` fetches the listing (free) on every run under `--analyst jev`,
  `--reviewer jev`, or `--explorer jev`, and fails the campaign before it starts on a mismatch,
  naming `TestLiveBaseline`. An unreachable or malformed listing only ever warns: not knowing the
  date is not evidence the model drifted.
- `Preflight`'s one request is now the canary (`internal/jev/canary.go`) rather than a plain
  connectivity question: a fixed synthetic function and all seven cause questions
  (`causes.go`'s own text, so a reworded question forces a re-measure here too), with recorded
  answers measured once (`TestLiveCanary`, 6 repeats) and guarded by a digest exactly like
  `TestBaselineMatchesQuestions`. An answer that moved by more than `CanaryTolerance` (0.05) fails
  the preflight, naming `TestLiveCanary`. The measured per-answer sd was 0.006–0.015, so 0.05 is
  at least 3.3 sd.
- The canary function is chosen for mid-range answers. An answer near 0 or 1 barely moves when
  the model changes, and a first draft (a nested-loop `sumPairs`) answered 0.02–0.085 on four
  questions and 0.98 on the fifth, which made it nearly blind. The shipped function (read,
  optionally sort, format and print entries) answers 0.16–0.85, five of seven between 0.25 and 0.78.
- Both drift guards — the release date and the canary — are downgraded from a failure to a warning
  by `GOTORQUE_JEV_ALLOW_DRIFT` (any non-empty value). The provider pin has no such override: no
  baseline or canary value here is valid for a provider other than TypeSafe's own, so accepting a
  wrong one would not be "running with known drift", it would be silently comparing today's answers
  against numbers that were never measured against what answered.

## Consequences

A campaign under any `--*  jev` flag now spends its one preflight request on the canary instead of
a connectivity check, at the same cost, plus one free HTTP GET. A version or provider change is
caught before repository work starts rather than discovered later as an unexplained shift in cause
or hazard rankings. `GOTORQUE_JEV_ALLOW_DRIFT=1` exists for a deliberate run against known drift (for
example, while re-measuring `TestLiveBaseline` after a release) without editing code.

This still does not pin a model version — that remains TypeSafe's own API, per ADR 0012 — and the
canary covers only the cause questions: a release that moved only the reviewer's hazard questions
or the fix-kind questions would pass both guards. The release-date check is opportunistic: its only
basis is that the field existed and held a plausible date at measurement time, not a documented
guarantee that TypeSafe changes it on every release.

## Alternatives considered

- Switching to TypeSafe's own `POST /v1/systemone`, which accepts pinned version IDs: still needs a
  second account and key, and its `noul` answer shape is not what `internal/jev` decodes today; ADR
  0012 already named this as the real fix and deferred it.
- A canary adding the hazard and fix-kind questions to the seven cause questions: triples the
  request cost of the one preflight call for coverage this ADR's evidence did not show was needed.
- Refusing a response with no routing metadata at all, on the theory that the pin should be
  provably in effect: rejected, because a stub or a gateway response shape change would then read as
  a wrong provider, which it is not.
