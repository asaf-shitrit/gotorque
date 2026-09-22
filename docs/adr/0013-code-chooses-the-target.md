# 0013. With causes ranked, code chooses each candidate's target

- Status: accepted (applies with `--analyst jev`)
- Date: 2026-09-22

## Context

The first live gron campaign run with the Jev analyst and caller-attributed discovery did what the
benchmark promised: discovery put the output loop (`main.go:206`) first, Jev flagged it for
unbuffered IO at +3.3 sd, and the analysis listed "route the writes in gron through bufio" as its
first hypothesis. The optimizer did not take it. Given the ranked list plus the coordinator model's
free-text plan, its first candidate split an index check out of `validIdentifier`'s loop, a cause
Jev had not even flagged for that function, and measured -0.85%, unsupported. Only its second
candidate took the top target, and that one was accepted: -15.1% wall time, -22.2% on
`flatten-users`, eight minutes into the campaign.

The coordinator model, meanwhile, spent between 12 s and 2m40s per cycle writing a plan whose only
consumer was the optimizer's prompt, and it runs before the analysis it would need.

## Decision

When the analysis ranks causes, `merge_analysis` chooses the target in code: the first (location,
cause) in the analyst's `targets` that no earlier candidate tried. It writes that target's remedy as
the coordinator's experiment, reduces the source excerpts to the target function, and the optimizer's
instruction forbids patching anything else. Because patches must anchor to the supplied excerpts,
the optimizer cannot drift to another function. The coordinator node becomes a deterministic stub in
this mode. Each verdict records its target; resume reads them back from the candidate records.

## Consequences

Each candidate attacks the next target in Jev's order, so a campaign walks the ranking instead of
revisiting favourites, and a verdict can be read against the cause it tested. The report names the
target for every attempt. Once every flagged target has been tried, the optimizer chooses freely
again, as it does with a model analyst, whose path is unchanged. The optimizer still decides how to
write the patch; it no longer decides where.

## Alternatives considered

- Ask Jev whether a new hypothesis repeats a rejected one: unnecessary, because code chose each
  target and knows exactly which were tried.
- Keep the coordinator model and feed it Jev's ranking: still a slow call, still free text the
  optimizer may not follow, which is the failure observed.
- Order targets by Jev's z-score instead of hotness: the hottest function's best-supported cause is
  the likeliest to move wall time, and on gron the accepted patch was the top target in hotness order.
