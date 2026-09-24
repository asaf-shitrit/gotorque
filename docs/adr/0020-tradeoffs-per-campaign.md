# 0020. A campaign may pick its own trade-offs

- Status: accepted
- Date: 2026-09-25

## Context

A target's manifest fixes what a campaign improves and how far each other metric may regress. The
shipped manifests improve wall time and allow 2% more peak memory and CPU time. A live go-jsonnet
candidate cut wall time 3.47% and CPU time 2.42%, both supported, and was rejected for 2.43% more
peak memory. The rejection was right under that contract, but whether an interpreter should trade
memory for speed is the operator's call for a given campaign, not something the manifest author
settles for every run. Changing it meant editing a checked-in manifest.

## Decision

`gotorque optimize` takes `--tradeoff balanced|speed|lean` and a repeatable `--allow
metric=percent`. A preset names the metric to improve and the allowances; `--allow` overrides or adds
one metric's limit on top. `balanced` is the manifest as written. `speed` improves wall time and
allows memory +10%, CPU +5%. `lean` improves peak memory and allows wall and CPU time +3%.

The trade-off is resolved once, when the campaign is created, into the campaign's copy of the
manifest. Switching the improved metric turns the old one into a required guardrail at the
manifest's guardrail limit. An allowance makes its metric a required guardrail. A metric cannot be
improved and allowed to regress at once. The trade-off is recorded in state. A resume keeps it and
refuses new trade-off flags. The report opens with a "Judged under" line and reproduces the flags.
Bad flags fail before any provider is called.

The policy is unchanged: it reads the performance block it always read.

## Evidence

`TestTheSpeedTradeoffAcceptsWhatTheManifestRejects` replays the go-jsonnet candidate's readings
through the real policy with go-jsonnet's shipped manifest. The manifest's limits reject it for peak
memory, `--tradeoff speed` accepts it, and `--tradeoff speed --allow memory=2%` rejects it again.

## Consequences

The acceptance bar for a campaign is now what the operator chose, so a report without its "Judged
under" line would be ambiguous; the line is always written. Discovery still profiles CPU, so a `lean`
campaign's targets come from CPU-hot code and find memory wins only where that code also allocates.
Choosing targets for memory needs an allocation profile, and is left for later.
