# 0040. Seeds that cannot accept are measured, and can only veto

- Status: accepted
- Date: 2026-10-24
- Relates to: [0002](0002-verdicts-may-rest-on-any-eligible-reading.md) (which readings a verdict may rest on), [0016](0016-regressions-reject-on-significance.md) (a regression rejects on significance), [0036](0036-candidate-evaluation-is-its-own-module.md) (where the measurement lives)

## Context

`docs/target-manifest.md` has described three workload tiers since the manifest
was written. `representative` seeds were "the only workloads eligible to
establish a performance acceptance", `plausible` seeds "add evidence, but
cannot headline an acceptance", and `stress` seeds "can expose scaling, safety,
or memory regressions and veto a candidate, but cannot accept one alone". The
manifest schema required `tiers.<tier>.weight` and
`tiers.<tier>.acceptance_eligible` for all three tiers.

No code read either field. Three loops, one per consumer of a seed, skipped every
seed that was not `representative`:

```
internal/campaign/candidate_eval.go   measureSeedWorkloads: if seed.Tier != domain.TierRepresentative { continue }
internal/campaign/candidate_eval.go   measurePgoWorkloads:  same
internal/campaign/verify.go           verifyOutputs:        same
```

The candidate binary was therefore never run on a plausible or stress seed.
Consider gojq: its `large-array` stress seed says in the manifest that "stress
evidence cannot accept a patch alone", which implies it can reject one, and the
campaign never executed it. A patch that returns a wrong sum for arrays longer
than the representative inputs passed the behavior comparison and was judged on
representative timings alone. Thirty-one checked-in manifests declare at least
one such seed. `docs/architecture.md` described the behavior accurately ("only
those seeds are measured"), so the two documents contradicted each other and no
ADR recorded a decision to keep it that way.

## Decision

1. `manifest.Manifest.AcceptanceEligible(tier)` is the one reader of the tier
   policy, and the two measurement paths key off it rather than off the tier's
   name. A seed whose tier is acceptance-eligible is measured for the verdict
   (`measureSeedWorkloads`); every other seed is measured to veto
   (`measureStressSeeds`). A manifest that flips the field moves a seed from one
   pass to the other, and no seed is measured twice.
2. The veto pass keeps two things and nothing else. Behavior is compared the way
   every other measurement compares it (`runSeries` and `recordBehaviorMatch`),
   so a different exit code or stdout on such a seed rejects the candidate. The
   guardrails `cpu_time_ns` and `peak_memory_bytes` are compared per workload
   and handed to the policy as `Evidence.StressComparisons`. The pass runs
   inside `measurePass`, the one pass the contention re-measurement repeats, so
   a load burst during a stress series is discarded with the eligible seeds'
   readings instead of rejecting a candidate on noise.
3. `wall_time_ns` is not compared on those seeds. A workload the manifest keeps
   out of acceptance must not be able to argue for one, so the primary metric is
   absent from the readings by construction.
4. `policy.Evaluate` rejects when such a reading is over its guardrail limit and
   significant, or over the limit alone when the manifest does not require
   statistical support. That is the rule ADR 0016 set for every other reading. A
   reading that stays insignificant is named in the verdict's reasons instead,
   because these workloads have no confirmation series of their own: a second
   series on every stress seed would double the cost of the pass for a reading
   the policy will not act on.
5. The readings are persisted on the candidate record and rendered in the
   report, so a veto and a named reason can be audited from the files a campaign
   leaves behind.

## Consequences

- A candidate is now measured on every seed the manifest declares, so a campaign
  pays one more interleaved series per non-acceptance-eligible seed. Stress
  inputs are larger than representative ones by construction; on the checked-in
  manifests that is one to four extra series per candidate, and the campaign's
  `max_duration` bounds them as it bounds everything else.
- The stress veto is real, and the manifest documents it truthfully again. A
  behavior break that only a large input reaches now costs a rejection rather
  than an accepted false positive, which is the direction goal 1 asks for.
- A null-candidate run measures the same way, because it reaches the same
  evaluation loop, so a false-accept rate measured on nulls now also covers the
  stress pass. A null candidate that a stress guardrail rejects is still not a
  false accept; the cost is the extra series.
- `tiers.<tier>.weight` is still unread. It stays in the schema so a version-one
  manifest keeps loading, and `docs/target-manifest.md` now says it does
  nothing. Weighting measurements would change the pooled comparison itself, and
  nothing in this change needs it.
- `verifyOutputs` and the informational PGO lane still run on
  acceptance-eligible seeds only. `verifyOutputs` compares input variants of a
  seed rather than the seed's own tier role; the PGO lane attributes a compiler
  effect and never changes a verdict.

## Alternatives considered

- **Document that only representative seeds are measured** (option b of issue
  #101). It is cheap and honest, but it gives up a documented safety property the
  harness already claims, and goal 1 measures false accepts. Rejected by the
  human on 2026-10-24 with "Decision: approved, go ahead and fix."
- **Let a stress seed carry an acceptance too.** The docs say it cannot, and a
  single extreme input is not the common case the 3% threshold is about.
- **Veto on wall time as well as the guardrails.** A stress seed's timing is the
  least reproducible measurement the harness has, and letting it reject turns
  noise into false rejections; the manifest names scaling, safety and memory,
  not wall time. The guardrails cover memory and CPU.
- **Give the stress series a confirmation pass.** It would make an insignificant
  guardrail reading actionable at twice the cost, for a reading that only ever
  rejects. Naming it in the reasons keeps it auditable until the evidence
  supports acting on it.
