# 0008. A campaign that spends its budget reports it, and an absorbed role failure is recorded

- Status: accepted
- Date: 2026-09-20

## Context

Two losses on the same path. A campaign that spends its `max_duration` never reaches
`finishCampaign`, so the only artifact was a mid-run snapshot: gron-9 ran its full ninety minutes and
its report still said `running`, with no stop reason and `token_usage: null`, while the database held
`max_duration 1h30m0s spent`. And a role whose model call failed was absorbed by the graph with its
cause written to the process's stderr — not the progress writer the campaign was given — so a
candidate that arrived empty was explained only as `patch is empty`.

## Decision

Write the report files when a campaign stops for any reason, snapshot per-role usage on every exit
from the ADK run, and record absorbed role failures in campaign state so the report can name them
alongside their cause.

## Consequences

An interrupted campaign is readable and its cost is accounted. `patch is empty` is read next to the
stall that caused it, in a `Degraded roles` section above the affected candidates.

## Alternatives considered

Rely on completion-time reporting (rejected: spending the budget is a normal outcome its own bound
produces, now more often since the patch budget is spendable). Keep writing to stderr (rejected: no
report and no API consumer can read it).
