# 0044. Discovery samples every acceptance-eligible seed, weighted by its tier

Status: accepted

## Context

Discovery sampled one workload. `Ladder.FirstLiving` ran the manifest's first
seed, and only if that could not be sampled at all did it try a stress-tier
seed, so that some evidence existed. Every other seed in the manifest was a
measurement workload and nothing more: the campaign's verdicts were taken from
all of them, but the hot list that chooses what to optimize came from the first
one alone.

A representative seed that runs a different code path therefore could not make
its hot spot a target. actionlint declares two representative seeds,
`lint-oneline` and `lint-json` (`-format '{{json .}}'`). Unpatched, `lint-json`
scales quadratically — 0.12 s at 800 workflow steps, 9.35 s at 8,000, against
0.28 s for `-oneline` — because `(*Error).getLine` rescans the source from line
one with a fresh `bufio.Scanner` for every error, and the JSON template reads
each error's snippet. Discovery sampled `lint-oneline`, the hot list named
`error.go:96`, `expr_lexer.go:218` and `rule_expression.go:775` but not
`getLine`, and all four attempts of the first actionlint campaign went at
oneline-path functions: one accept and three inconclusive.

The manifest's tier policy already says which readings may accept a candidate
(`workloads.tiers[*].acceptance_eligible`, ADR 0002) and carries a weight per
tier (`workloads.tiers[*].weight`), but nothing read the weight: `tiers` was
validated at load time and used for nothing else.

## Decision

Discovery samples every acceptance-eligible seed, not only the first one, and
merges the samples weighted by each seed's tier weight.

- `discovery.Inputs.Tiers` carries the manifest's tier policy. A seed whose
  tier is `acceptance_eligible` is sampled next to the first seed; a seed whose
  tier is not (a plausible or stress seed) reaches no campaign reading and is
  not sampled for its own sake.
- `mergeAttributed` keeps each sample's attributed weights as fractions of that
  sample's total, which normalizes for how long the sample ran, and multiplies
  each by the sample's tier weight before summing. The merge is therefore
  `sum over samples of (tier weight × this sample's share of that function)`.
- Each sample's attributed share is computed once, in `attributedShares`, so the
  loop that applies the weight stays simple enough for the complexity gates.
- When no sample carries a weight — a manifest that weights every tier at zero,
  or an `Inputs` with no tier policy — every sample counts equally. An all-zero
  merge would rank the hot list by function name.
- A stress seed that the ladder samples because the first seed cannot be
  sampled at all stands in for that first seed and carries *its* weight. The
  stress weight of zero is a statement about acceptance, not about the value of
  a profile that is the campaign's only evidence.
- An explored variant runs the sampled seed's own input in another mode, so it
  carries the seed's weight rather than a weight of its own.
- `Evidence.SeedSamples` records which seeds were sampled and the weight each
  carried. The engine persists it as `State.DiscoverySeedSamples` and the
  report prints it as one `Discovery seeds:` line, so the hot list's
  provenance is auditable: a function can rank because a seed other than the
  first reached it.
- A seed that is acceptance-eligible but cannot be sampled is recorded
  (`workload_sample_skipped`) and left out. The samples that did run still make
  a hot list, and discovery does not fall back to benchmarks because one seed
  failed.
- Benchmark fallback, the PGO lane, the unbuffered-write evidence and the
  allocation profile are unchanged.

## Consequences

- Discovery costs one sampling window per extra acceptance-eligible seed: about
  four seconds each, plus a retry on a larger input when the target exits too
  soon. Most targets declare two or three representative seeds; `csvtk`,
  `miller`, `minify` and `seqkit` declare five. `discovery_stall_timeout` (20
  minutes for gojq) still bounds the step.
- Hot lists change. A function reached only by a later acceptance-eligible seed
  now enters the list and can become a target, which is the point; functions
  that only the first seed reached can be pushed out of the 15-function budget
  by the merged ranking.
- A campaign recorded before this change has no `discovery_seed_samples`, and
  the report omits the line. Resuming one re-runs only an unfinished discovery
  step, so a resumed campaign keeps the hot list it already had.
- The merge stays deterministic and model-free: tiers, weights and eligibility
  are manifest data, and nothing about the ranking is a model's judgment.

## Alternatives considered

- **Equal weights for every sampled seed.** Simpler, and it is what the merge
  does today for seed and variants. But it gives a plausible-tier workload the
  same influence as one whose readings can accept, and it would leave
  `tiers[*].weight` still unread.
- **Sample every seed, including plausible and stress ones.** It profiles code
  no acceptance reading exercises, which is where a patch cannot show up in the
  measurement, and it grows discovery by two or three more windows.
- **Weight by measured wall time instead of tier.** The tier weight is the
  manifest's own statement about which readings matter; wall time would let one
  long workload dominate the hot list, which is the failure the per-sample
  fraction normalization already prevents.
- **Cap the number of sampled seeds (as `maxExploredWorkloads` caps variants).**
  A cap silently leaves a seed's code paths out of the profile, which is the
  bug this decision fixes, and a manifest author cannot tell which seed was
  dropped. The stall timeout is the bound instead.
