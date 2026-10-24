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
rule ADR 0033 set for an insignificant reading. The fresh readings are recorded
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
