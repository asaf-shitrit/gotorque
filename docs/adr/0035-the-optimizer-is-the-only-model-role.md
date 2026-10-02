# 0035. The optimizer is the only model role

- Status: accepted
- Date: 2026-10-02

## Context

The campaign graph had five model roles: coordinator, explorer, analyst, optimizer and reviewer.
ADRs 0012, 0014 and 0015 added Jev alternatives for the analyst, reviewer and explorer behind
`--analyst jev`, `--reviewer jev` and `--explorer jev`, and the model versions stayed beside them.
ADR 0013 already made code, not a model, choose the target, and the coordinator model was skipped
whenever the analyst was Jev.

Every sweep and the daily gap hunt ran `--analyst jev --reviewer jev --explorer jev`. The model
paths for those roles were untested in practice while still carried in code: prompts, tolerant
decoders for their answers, proposal validation, per-role routing and reasoning variables, and the
circuit breaker's counting of which roles were model-served.

## Decision

Remove them. The optimizer is the only model role. The analyst, reviewer and explorer are always
Jev, with their questions ranked in code, and the coordinator is code (`planCoordinator`). The
explorer node is code too (`planExplorer`); it reports the variants the Jev explorer chose before
discovery.

- `--analyst`, `--reviewer` and `--explorer` are gone. `--adk` means the optimizer model through
  OpenRouter plus Jev and needs `OPENROUTER_API_KEY`; `--adk-stub` means a stub optimizer plus
  `jev.Stub`, with no network.
- Routing reads `GOTORQUE_MODEL_OPTIMIZER` (default `deepseek/deepseek-v4.1-flash`) and
  `GOTORQUE_REASONING_OPTIMIZER` (`low|medium|high`, defaulting to `low`, as code always chooses
  the target). The other `GOTORQUE_MODEL_*` and `GOTORQUE_REASONING_*` variables no longer exist.
- The breaker counts only the optimizer: two consecutive failed cycles stop the campaign as
  failed. A Jev failure degrades its role and never trips it.
- `run_discovery` no longer validates explorer proposals; `internal/workload` only reads the
  target's boolean options.

## Consequences

- About a thousand lines are deleted: the role prompts, tolerant decoders, proposal validation,
  per-role routing and the role-counting in the breaker.
- A campaign that ran model roles (`ADKMode` set, analyst not `jev`) cannot be resumed. Resume
  refuses it with "ran model roles this build no longer has; start a new campaign". Its report
  still reads.
- The graph keeps the coordinator and explorer stage nodes, now code. Collapsing them into their
  neighbours is deferred.
- Older ADRs that introduced the flags (0012, 0014, 0015, 0030) stay as written; they are the
  record of why each Jev role exists.

## Alternatives considered

- Keep the model roles as an escape hatch if Jev is unavailable. Rejected: a path nothing runs is
  not a working fallback. When Jev is down its role degrades (discovery's hot paths, no review
  concerns) and the campaign continues.
