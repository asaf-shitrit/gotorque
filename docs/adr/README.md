# Decisions

One file per structural decision, in the order they were taken. Each records the evidence that
forced it and the alternatives considered, so a later architecture survey does not re-suggest
something already declined. `docs/architecture.md` describes the current state;
`campaign.ReportSchemaVersion` records the shape of written reports.

| ADR | Decision | Status |
| --- | --- | --- |
| [0001](0001-workload-identity-is-structured.md) | Workload identity is structured, not spelled into a name | accepted |
| [0002](0002-verdicts-may-rest-on-any-eligible-reading.md) | A verdict may rest on any acceptance-eligible reading | accepted |
| [0003](0003-manifest-performance-block-is-the-contract.md) | The target manifest's performance block is the acceptance contract | accepted |
| [0004](0004-measurement-power-and-benchstat-lane.md) | Measurement uses 25 interleaved pairs and a benchstat lane verified against the real binary | accepted |
| [0005](0005-behavior-gate-subtracts-preexisting-failures.md) | The behavior gate subtracts pre-existing failures and refuses unattributable baselines | accepted |
| [0006](0006-model-calls-stream-bounded-by-silence.md) | Model calls stream, and silence — not total duration — is the bound | accepted |
| [0007](0007-pgo-lane-is-budget-bounded.md) | The informational PGO lane cannot spend a verdict's budget | accepted |
| [0008](0008-budget-spent-campaigns-report-themselves.md) | A campaign that spends its budget reports it, and an absorbed role failure is recorded | accepted |
| [0009](0009-deferred-evaluation-module-seam.md) | Deferred: give candidate evaluation its own module seam | deferred |
| [0010](0010-verify-model-availability-before-routing.md) | Verify a model is available before a campaign spends on it | accepted |
| [0011](0011-no-upstream-proposals.md) | No upstream proposals from this effort | deferred |
| [0012](0012-jev-cause-analyst.md) | Jev cause classification may replace the analyst model | accepted (opt-in) |
| [0013](0013-code-chooses-the-target.md) | With causes ranked, code chooses each candidate's target | accepted (opt-in) |
| [0014](0014-jev-reviewer.md) | Jev behaviour-hazard checks may replace the reviewer model, and reviews are kept | accepted (opt-in) |
| [0015](0015-jev-explorer.md) | Code-generated option variants, judged by Jev, may replace the explorer model | accepted (opt-in) |
| [0016](0016-regressions-reject-on-significance.md) | A regression rejects only when it is significant, after a second series if needed | accepted |
| [0017](0017-patch-shape-check.md) | A patch is held to its shape before it is built, and an unmeasured target is retried once | accepted |
| [0018](0018-targets-weigh-evidence-against-hotness.md) | Targets are ordered by evidence discounted by hotness | accepted |
| [0019](0019-fix-kinds-and-in-memory-writers.md) | Jev picks the fix within a cause, and code overrules unbuffered-I/O flags on in-memory writers | accepted (opt-in) |
| [0020](0020-tradeoffs-per-campaign.md) | A campaign may pick its own trade-offs | accepted |
| [0021](0021-improvements-are-measured-again.md) | A promising improvement is measured again, mirroring a regression | accepted |
| [0022](0022-optimizer-returns-function-source.md) | With a code-chosen target, the optimizer returns the function's new source, and code builds the diff | accepted |
| [0023](0023-nested-module-build-directory.md) | A target's build may run from a nested module directory | accepted |
| [0024](0024-memory-objective-targets.md) | A memory objective chooses memory-relevant targets | accepted |
| [0025](0025-flag-on-yes-veto-and-defer-fast-path.md) | A cause is flagged only when Jev says yes; code vetoes what it can rule out; fast path goes last | accepted |
| [0026](0026-manifest-sandbox-is-enforced.md) | The manifest's `sandbox` block is enforced, not just documented | accepted |
| [0027](0027-throwaway-result-multi-function-targets.md) | A throwaway-result signal picks a multi-function target, and the optimizer returns several function sources | accepted |
| [0028](0028-jev-answer-cache-and-batched-fix-kinds.md) | Jev answers are cached per campaign, and fix-kind questions are batched into the cause request | accepted |

Statuses: `accepted` (in force), `deferred` (deliberately not done — revisit with new evidence).
