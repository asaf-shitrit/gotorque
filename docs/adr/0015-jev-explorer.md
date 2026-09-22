# 0015. Code-generated option variants, judged by Jev, may replace the explorer model

- Status: accepted (opt-in, `--explorer jev`)
- Date: 2026-09-22

## Context

The explorer was a model call per cycle that proposed workload strategies. `run_discovery` validated
each proposal and counted it, but nothing ran one, so discovery only ever sampled the manifest's
first seed. A CLI's other modes stayed out of every profile: gron's `--stream` runs `gronStream`,
a hot path the seed never reaches, and no analyst was asked about it.

Which options a program has is in its source. Whether one changes the work the program does is a
yes/no judgment about the program's own help text, which is what Jev answers. Whether a variant
actually runs and does something different is a fact code can check.

## Decision

Offer `--explorer jev`. Before discovery samples anything:

1. Code lists the boolean options the target's source declares (standard `flag`, pflag/cobra,
   go-flags tags), with spellings bound to one variable folded together, and skips those the seed
   already passes.
2. Jev answers one yes/no question per option in a single request whose state is the command and
   its `--help` output: does the option change how the program processes its input or formats its
   output. An option at probability one half or more is a processing mode.
3. Code runs each mode once on the seed input, likeliest first, and keeps up to three that exit 0
   with output different from the seed's.
4. Discovery samples each kept variant after the seed and merges the samples, weighing each as a
   whole. A variant whose target exits before the sampler attaches on the seed's amplified input is
   sampled on the seed input repeated one copy per line.

The explorer node then answers at once with that plan instead of a model call.

## Evidence

Jev on the options of two targets, with no baseline and the floor at one half:

| Target | Options | Judged a mode | Below one half |
| --- | --- | --- | --- |
| gron | 9 | 7, at 0.89 to 0.97 | `--version` 0.10, `--insecure` 0.12 |
| gojq | 17 | 13, at 0.89 to 0.98 | `--from-file` 0.49, `--exit-status` 0.15, `--version` and `--help` 0.09 |

An earlier wording asked whether a typical user passes the option. Jev answered that honestly (most
users pass none) and put every gron option below one half except `--ungron`, which fails on JSON
input.

On a gron campaign the kept variants were `--stream` (0.97), `--json` (0.95) and `--no-sort`
(0.93). `gronStream` entered the hot list third, the `--json` path (`jsonify`,
`statementsFromJSON`) entered it too, and the two standard-library names that had filled its tail
dropped out. The first attempt sampled `--stream` on the 16 MiB single-line document the seed is
amplified into; gron's stream scanner rejects lines over 1 MiB, the target exited at once, and the
variant added nothing, which is why a variant falls back to line-repeated input.

## Consequences

Discovery costs one Jev request, one `--help` run, a run per mode tried, and up to three extra
sampling windows of about five seconds each. The variants only widen the hot list the analyst and
target choice work from; they are never measured workloads and no verdict reads them. Jev's
probabilities for real modes cluster between 0.9 and 1, so they order the modes only loosely and
ties keep declaration order; the output check, not the probability, decides what is sampled.
Non-boolean options, such as gojq's `--arg` or `--indent`, are not explored. A variant runs with
the seed's own input, so a mode that needs a different input format, such as `--ungron` on gron's JSON
seed, fails the check and is dropped.

## Alternatives considered

- Run the explorer model's proposals: they describe strategies in prose, and turning them into
  runnable workloads needs the same option discovery and output check, with a model call in front.
- Sample every option: gojq has seventeen, and help, version and network options reach no new code.
- Let Jev choose without the output check: an option can be a mode in general and still fail on
  this input (`--ungron`, judged 0.97, exits non-zero on JSON) or change nothing on it.
