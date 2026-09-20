# 0007. The informational PGO lane cannot spend a verdict's budget

- Status: accepted
- Date: 2026-09-20

## Context

A gron campaign ended with no verdict at all: its event log shows the candidate's measurement
finishing at 15:02 and nothing else until 15:38, when the PGO lane reported that its baseline build
failed on an expired context — one profile-guided build had run for thirty-six minutes inside a
forty-minute campaign, and the deadline that killed it took the already-measured candidate with it.

## Decision

Bound each `-pgo` build at five minutes, skip the lane when the campaign has fewer than three such
budgets left, and report a build cut short by that bound as the lane's own cost rather than a
compiler failure.

## Consequences

The lane stays informational and can no longer cost a verdict. A slow target degrades to a
recorded skip.

## Alternatives considered

Remove the lane (rejected: it attributes the compiler's profile-guided effect honestly). Leave it
unbounded (rejected: it demonstrably cost a whole campaign).
