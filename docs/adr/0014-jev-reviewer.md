# 0014. Jev behaviour-hazard checks may replace the reviewer model, and reviews are kept

- Status: accepted (opt-in, `--reviewer jev`); recording applies to every reviewer
- Date: 2026-09-22

## Context

The reviewer was a model call per candidate that walked a behaviour-hazard checklist and returned
`proceed`, concerns and required checks. Nothing used the answer: it reached a policy input the
policy ignores, and neither the candidate record nor the report kept it. On gron campaigns the call
frequently timed out. Meanwhile both gron patches gotorque accepted wrap stdout in a bufio writer and
discard the flush error (`defer bw.Flush()`), which a reviewer exists to point out.

Its checklist is a list of yes/no judgments about one diff, which is what Jev answers.

## Decision

Offer `--reviewer jev`: a deterministic `reviewer` node asks Jev one question per hazard (output
order, error behaviour, dropped error, skipped effect, buffer aliasing, concurrency, numeric output,
off-target change) about the hypothesis, the diff, and the patched function's source at the base
revision. A hazard is raised when Jev answers yes (p >= 0.5) and the answer sits at least two
standard deviations above its usual answer on 93 real, merged performance patches.

Record every review's concerns with the verdict, in the report, and in the next cycle's prior
candidates, whichever reviewer produced them.

## Evidence

A benchmark of the 93 real patches plus gron functions with one hazard injected per variant:

| Rule | Injected hazards caught | Real patches with a concern |
| --- | --- | --- |
| z >= 2 | 11/11 | 31% |
| z >= 2 and p >= 0.5 | 10/11 (the miss was a mislabelled case) | 14% |

The mislabelled case: a shared `strings.Builder` was labelled as aliasing, but `Reset` drops the
buffer rather than reusing it, and Jev answered 0.39; it caught that variant's real hazard,
concurrency, at +22.8 sd. Most concerns on real patches are genuine, including dropped-error
p = 0.96 and skipped-effect p = 0.84 on the patch gotorque accepted.

## Consequences

A review costs one request of about a second instead of a model call that often timed out, and its
concerns reach the report and the next patch. The injected hazards are one or two per class, so
detection per class is a smoke test rather than a measurement; the false-alarm rate on real patches
is the solid number. The baseline is tied to the question text and review state
(`TestReviewBaselineMatchesQuestions`) and, like the cause baseline, cannot be pinned to a Jev
version through the gateway.

## Alternatives considered

- Keep the model reviewer and only start recording it: recording is adopted for both, but the call
  itself stays slow and often degraded to nothing.
- Let a raised hazard reject the candidate: rejected. The behaviour gate and policy own the verdict,
  and a model output must not end a candidate.
- Flag on z alone: raised a concern on almost a third of real merged patches.
