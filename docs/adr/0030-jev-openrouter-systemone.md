# 0030. Jev is reached through OpenRouter's System One API, pinned to an exact build

- Status: accepted
- Date: 2026-09-27

## Context

The Vercel AI Gateway account this codebase used for Jev stopped answering: every
`POST /v1/evaluate` call now returns HTTP 403, "Free tier users do not have access to this model."
That path is gone, not degraded, so there is nothing left to guard against drift on.

Jev is served by OpenRouter instead, whose System One API (`POST /systemone`) accepts a fully
pinned build id as `model` — `typesafe/jev-1.13-20260917`, confirmed live alongside the version
alias `typesafe/jev-1.13` and the alias `~typesafe/jev-latest`. This is the fix ADR 0012 already
named and deferred, and it closes the exact gap ADR 0029 could only work around: that ADR's
`providerOptions.gateway.only` pin and its free `GET /typesafe/v1/models` release-date probe both
existed only because the gateway's `typesafe-ai/jev` alias carried no version and could silently
route to a second upstream. Neither problem exists on System One: the request itself names the
build, and every response's `provider` field names who actually answered (`TypeSafe`, confirmed on
every response in this migration's live measurement).

The request and response shapes differ from the gateway's, not just the endpoint:

- Questions are typed `noul`, `choice`, or `score`; `boolean` — everything this codebase's own
  `boolean()` helper built — is rejected outright with HTTP 400. The true/false criteria this
  package already writes are unaffected; only the `type` field sent on the wire changes to `noul`.
- A `noul` answer carries its probability under `"noul"` (`{"type":"noul","noul":0.91}`), not
  `"probability"`. Answers are persisted verbatim in the bbolt `jev_cache` and in campaign events
  under the older field name from the gateway era, so decoding both without special-casing which
  store an `Answer` came from mattered more than picking one wire name.
- Usage fields are `input_tokens`/`output_tokens` (snake_case, not the gateway's camelCase), plus a
  `cost` in US dollars that the gateway never reported (System One prices Jev's output tokens free).
- The response carries no `providerMetadata.gateway.routing` structure to inspect; it states
  `provider` directly.

Every baseline and the canary are digested over the exact question set sent, and the type change
alone (`boolean` -> `noul`) changes every digest, so all of them needed re-measuring regardless of
whether the underlying model changed. They were re-measured live against the pinned build,
confirmed served by `TypeSafe`: the cause baseline and the fix-kind baseline together (186 real
functions, one combined request per function, matching what production already sends per ADR
0028), the review baseline (93 real merged patches), and the canary (6 repeats). A full comparison
against the old gateway-served answers (paired by id, same functions and patches) found every
question's mean moved by under two percentage points, an overall paired mean |Δ| of 0.010 for
causes and 0.016 for hazards, the top-ranked cause changing for 9/186 sites (4.8%) and the ADR 0025
flag set changing for 21/186 (11.3%), the hazard flag set changing for 10/93 (10.8%), and the
canary still landing mid-range (0.16–0.85) with no answer saturating. The full report, with every
raw answer, is at `gotorque-work/jev-openrouter-2026-09-27/report.md`. Whatever previously served
those gateway answers — TypeSafe or a fallback the gateway never confirmed — was not meaningfully
different from what actually answers now.

## Decision

`internal/jev/client.go` posts to `OPENROUTER_BASE_URL + "/systemone"` (default
`https://openrouter.ai/api/v1/systemone`) using `OPENROUTER_API_KEY` — the same credential and
override the optimizer role already uses via `internal/agents`, so `--analyst jev`/`--reviewer
jev`/`--explorer jev` need no second account or key, unlike the gateway path ADR 0012 accepted as
a limitation. `Request.Model` defaults to `jev.Model` (`typesafe/jev-1.13-20260917`); `boolean()`
now builds `Type: "noul"` questions; `Answer.UnmarshalJSON` accepts either `"noul"` or
`"probability"`.

`Client.post` refuses an answer unconditionally, with no override, when a response's `model` is not
exactly `jev.Model` or its `provider` is not exactly `TypeSafe` (both checks skip a response that
carries neither field at all — a test stub, or a future shape change — since that is not evidence of
a wrong build or provider, just of not knowing). This replaces ADR 0029's `providerOptions`/
`finalProvider` gateway-routing metadata and its `GET /typesafe/v1/models` release-date check
outright: pinning the exact build in the request makes both unnecessary, and neither endpoint nor
concept exists on System One.

The canary (`internal/jev/canary.go`) survives unchanged in shape, still guarding what a build id
TypeSafe controls cannot: `GOTORQUE_JEV_ALLOW_DRIFT` still downgrades a moved canary answer to a
warning, since a build can still ship behind an id that answers differently than what was measured.
It has no bearing on the model/provider guard, which stays a hard failure — nothing recorded is
valid for a different build or provider, so accepting one silently would compare today's answers
against numbers never measured against what answered.

## Consequences

`--analyst jev`, `--reviewer jev`, and `--explorer jev` need only `OPENROUTER_API_KEY`, already
required for every other model role; `AI_GATEWAY_API_KEY` and `AI_GATEWAY_BASE_URL` are gone from
this codebase, along with the Vercel AI Gateway itself as a code path. The cause baseline, fix-kind
baseline, review baseline, and canary were all re-measured and their digests changed (the question
`type` field alone forces that); the measured numbers are in the tables above and in the full report
at `gotorque-work/jev-openrouter-2026-09-27/report.md`. Live spend for the entire re-measurement
(186 + 93 + 6 requests) was about $0.026, comfortably under System One's advertised
$0.042-per-million-input-tokens rate with free output.

This still does not remove every drift risk: the canary covers only the seven cause questions, so a
release that moves only the hazard or fix-kind questions behind the same pinned id would pass
preflight undetected, exactly as ADR 0029 already noted. That risk is now at least bounded by an id
TypeSafe controls rather than an alias with no version signal at all.

## Alternatives considered

- Keeping the gateway path as a fallback for when System One is unreachable: rejected — the gateway
  account cannot reach Jev at all any more, so there is nothing to fall back to, and maintaining two
  request/response shapes for one classifier bought nothing.
- Widening the canary to cover the hazard and fix-kind questions too, closing the gap noted above:
  deferred as out of scope for this migration; it triples the request cost of every preflight call
  for coverage no incident has yet shown is needed, the same trade-off ADR 0029 already declined.

## Addendum: the canary is confirmed before it fails

The first live campaign after this change (`openrouter-dasel-1`) stopped at the preflight. The
canary's `superlinear` answer came back 0.17 against a recorded 0.248, with the pinned build and
provider both confirmed. Ten fresh repeats put it at 0.235 (sd 0.014), so that answer was a
one-off outlier about 5 sd out, not drift. Jev's answers are mostly within the recorded ~0.01
noise but occasionally jump further.

So a drifted first canary answer is no longer final. `Preflight` asks the canary twice more
(`canaryConfirmations`) and judges each question by the median of the three answers. A single
outlier is outvoted, and a model that really moved still fails. Without drift the preflight stays
one request.

## Addendum: each canary answer gets its own tolerance

The median-of-three check did not end the false alarms. The overnight goawk campaign then stopped
at the preflight on a median `superlinear` of 0.19 against the recorded 0.248. More sampling
(6, 10, 12 and 20 repeats) showed why: the canary answers are not equally steady. `superlinear`'s
sd is about 0.026 and `redundant`'s 0.006, and the recorded 0.248 sat at the high end of
`superlinear`'s own range.

`TestLiveCanary` now records each answer's sd (`canarySpread`) beside its mean, re-measured over
20 repeats. A question's tolerance is the larger of 0.05 and four of its standard deviations:
0.103 for `superlinear`, 0.081 for `fast_path`, 0.079 for `unbuffered_io`, 0.067 for
`string_build`, 0.060 for `prealloc`, and the 0.05 floor for `alloc` and `redundant`.
