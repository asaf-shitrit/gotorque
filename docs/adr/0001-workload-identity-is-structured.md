# 0001. Workload identity is structured, not spelled into a name

- Status: accepted
- Date: 2026-09-20

## Context

A comparison carried one string that encoded two facts: `"<workloadID>/<metric>"` for a single
workload and the bare metric for the pool. Three packages agreed on that format — the engine built
it, the campaign re-parsed it with a suffix check to decide which comparisons a verdict could rest
on, and the policy parsed it again to name the workload. Both acceptance bugs found in that seam
(pooled-only acceptance, and the manifest's thresholds being ignored) lived in the assembly rather
than in the decision it fed, and the name a verdict printed was the derived run identifier:
`workload "aeb6767c20bae1e8fc17dd04/wall_time_ns" improved by 4.40%`.

## Decision

`domain.MetricComparison` carries `metric` (the canonical name guardrails key off) and `workload`
(the manifest seed id; empty means the pooled reading). Eligibility is structural (`metric ==
primary`), and the policy consumes the domain type directly.

## Consequences

`policy.Comparison`, `policy.ComparisonResult` and two conversion blocks are gone. Verdicts and all
three report tables name seeds the way an operator writes them (`workload "flatten-users" improved
by 3.26%`), and benchstat sample files are named by seed. Artifacts written before this render
`unlabelled` and say why.

## Alternatives considered

Keep the convention and add a label field (rejected: the convention stays a contract three packages
must agree on). A compatibility `name` field (rejected: the cut was deliberate; the artifact is
versioned instead, see ADR 0010's sibling decision in `report.go`).
