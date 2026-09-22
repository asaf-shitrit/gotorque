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

Statuses: `accepted` (in force), `deferred` (deliberately not done — revisit with new evidence).
