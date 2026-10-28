# 0040. A peak-memory breach is replicated in a fresh series before it rejects

- Status: accepted
- Date: 2026-10-24

## Context

ADR 0033 made a guardrail reject only on a significant difference, and gave an
insignificant over-limit reading a second series before the verdict. A breach
that is significant on its first series was final, and one series of pairs is
what decides it.

Peak memory (`peak_memory_bytes`) does not behave like CPU time at that
resolution. It is a per-run high-water mark, not a steady rate: it steps with
the allocator's size classes and with GC pacing, so it moves in megabytes
between measurements of binaries whose steady-state footprint did not change.
Two recorded cases:

- `daily-2026-10-02-heldout-tomlv`, attempt 4 (a sliced indent buffer,
  byte-identical output): the 25-pair campaign accepted on the primary metric,
  a 60-pair `verify` rejected on peak memory at +5.58% significant
  (36,507,648 → 38,546,091 B), a second 60-pair `verify` of the same binaries
  read -0.68%, and 30 interleaved `/usr/bin/time -l` runs put both binaries at
  about 30.4 MB.
- `daily-2026-10-03-heldout-sqlfmt`, attempts 2-4: three of four candidates
  were rejected on a significant first-series memory breach. Attempt 2 only
  replaced `buf.WriteString(fmt.Sprintf(...))` with direct `WriteString` calls
  — it removes allocations — and read +2.78% while the wall-time readings on
  the same series were supported improvements of about -3.4%.

ADR 0033 already accepts that a guardrail reading is noise until it is shown
to be otherwise, and accepts the risk of passing a small real regression to
avoid losing a supported win. A single significant reading of a high-water
mark does not show that; the same binary produces both numbers.

## Decision

A `peak_memory_bytes` breach that is over its limit and significant on the
first series does not decide the verdict on its own. The engine measures one
fresh series of pairs on every representative seed
(`policy.GuardrailsToReplicate` selects the breaches), records that series'
own pooled readings in `Evidence.FreshGuardrails`, and the policy rejects only
when those fresh readings also breach: over the limit, with support.

A breach the fresh series does not reproduce is named in the verdict's reasons
with both deltas and neither rejects nor makes the candidate inconclusive, the
rule ADR 0033 set for an insignificant reading. The reason quotes the readings
the first series produced, which the engine records in
`Evidence.FirstSeriesGuardrails` before the two series are pooled, because
`Comparisons` then hold one value for both. The fresh readings are recorded
(`fresh_guardrails` on the evidence, `fresh_comparisons` in the candidate
record) and the report shows them as their own table, so the verdict can be
re-read from disk.

It costs the same series ADR 0033 and ADR 0021 already allow, and the
one-extra-series-per-candidate bound still holds: the fresh series is the
confirmation series, whichever trigger asked for it.

CPU time and binary size keep the single-series rule. CPU time's noise is
per-run spread that more pairs average out (ADR 0033), and binary size compares
exact sizes.

A caller that supplies no fresh readings keeps the old rule, so the leniency
cannot be reached by forgetting to measure: `evaluate_candidate` and `verify`
both fill the field from the same evaluation path.

## Consequences

- A real memory regression still rejects: an allocation a patch genuinely adds
  appears in both series, and a consistent difference is significant even with
  no spread.
- A noisy breach costs one extra series and then neither rejects nor blocks.
  The wall-time win that motivated this ADR survives it.
- The false-rejection rate this should remove is not measured yet. The check
  is the null-candidate run (ADR 0033) with `peak_memory_bytes` reported
  separately before and after: comment-only patches should produce no
  memory rejections, and before this change they do.
- Alternatives declined: deciding on both series pooled (ADR 0034's analogue
  for accepts) leaves the contaminated series in the high-water mark, so the
  documented +5.58%/-0.68% pair would still read positive; an absolute floor in
  bytes is a second threshold with no measured size behind it — the null-run
  measurement above is what should choose it; recording GC cycle counts or
  GOGC pacing per side would explain a breach but not decide it, and the
  decision needs no more inputs than the two readings.

## Amendment (2026-10-25, #117): an over-limit reading is re-measured before it rejects

The decision above covers a breach that is significant on its first series. It missed the other
way the policy could reject on one series: a `peak_memory_bytes` reading over its limit without
significance already runs the confirmation series as an unconfirmed guardrail (ADR 0033), and
pooling that second series in can be what makes the pooled reading significant. The engine
recorded `Evidence.FreshGuardrails` only when `policy.GuardrailsToReplicate` selected a supported
breach, so on that path the policy found no fresh reading and rejected on the pooled series
alone — the single-series rule this ADR removed.

The engine now records the fresh series' own readings on every confirmation series, in
`confirmRegressions` and `confirmImprovements` alike, whether or not the first series was over the
limit. Reviewing the first fix found it recorded them only from `confirmRegressions`, and only for a
first series already over the limit: a reading that stayed under the limit and crossed it only once
pooled — the series a borderline accept starts, or an unconfirmed primary regression — still
rejected on the pooled series alone.
The policy rejects only when those fresh readings breach too, as for any replicated breach, and
the verdict's reason names a breach the fresh series did not reproduce either way: the first
series' own reading when that series already breached, or the pooled reading when it became
significant only once the fresh series was folded in. The extra series is the one the confirmation
already runs; no candidate costs a series it did not cost before.

## Amendment (2026-10-25, #109): a stress seed's peak-memory breach is replicated too

ADR 0046 gave the seeds the manifest keeps out of acceptance a veto, and first gave them the
single-series rule ADR 0016 sets for every other reading. That rule is wrong for peak memory there
for the reason it is wrong on the pooled readings: one series of a high-water mark can read a
breach the candidate did not cause, and a stress seed's only power is to veto, so a false veto is
the only harm it can do. The review of #109 named the case: a supported peak-memory regression that
the same binaries read back inside the limit one series later, which is what ADR 0040 exists to
prevent.

The engine now measures one fresh series on the stress seed whose peak memory breached
(`policy.StressGuardrailsToReplicate` selects it), records that series' own reading in
`Evidence.FreshGuardrails` under the seed id, and `policy.decideReplicatedBreach` — the one rule
both paths call — rejects only when the fresh reading breaches too. A pooled reading carries no
workload, so the two kinds share the field without ambiguity. A stress reading is never folded
with the fresh series: these workloads can veto and cannot carry, and their readings are recorded
one per series.

The cost is one series per breaching stress seed, paid only when a breach exists. It is not a
confirmation series in the sense of ADR 0021 and ADR 0033: it settles a veto, never an acceptance.
