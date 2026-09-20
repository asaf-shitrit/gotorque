# 0011. No upstream proposals from this effort

- Status: deferred
- Date: 2026-09-20

## Context

Two variants of one optimization to gron's `statement.String()` (an intermediate `[]string` plus
`strings.Join` replaced by a `strings.Builder`) were accepted by the policy and verified by hand on
rebuilt binaries: -3.55% (p=0.016) and -3.53% (p=0.0003) on the large-document seed, with the
upstream suite passing. They are semantically identical and differ only in hunk context.

## Decision

Keep the patches local as evidence; do not open upstream proposals from this effort.

## Consequences

Nothing external happens under the repository owner's identity. The patches and their campaign logs
live in `~/gotorque-results/gron-7/` and `~/gotorque-results/gron-8/`, so a later session can propose
them without re-running a campaign.

## Alternatives considered

Propose upstream now (declined by the owner: an external action needing their identity).
