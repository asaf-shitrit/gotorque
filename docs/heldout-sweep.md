# Held-out stability sweep

The procedure behind the README's Status claim: on the held-out targets
(`targets/HELDOUT.md`), every accepted patch holds when `verify` measures it
again, and null candidates produce no false accept. A sweep measures one named
gotorque revision. `scripts/heldout-sweep.sh` runs the steps below.

The rules in `targets/HELDOUT.md` apply to every step. Run on a committed
revision, never while the harness is being changed, and never fix a failure by
tuning against a held-out target: write it up, fix it on development targets,
then sweep again.

## What it costs

Only step 2 spends money. Steps 1, 3, 4 and 5 call no model and no API.

- Step 2 runs the optimizer model and Jev for every held-out target. Both read
  `OPENROUTER_API_KEY` and `OPENROUTER_BASE_URL`, so one gateway has to serve
  the optimizer's chat endpoint and Jev's System One endpoint. The free
  Experiential gateway chosen on #103 (`https://api.experientiallabs.ai/v1`,
  model `glm-5.3-uncensored`) is an OpenAI-compatible chat endpoint. Whether
  it also serves Jev is not known, and the code has no separate variable for
  the optimizer's key or URL. Settle that before step 2: either the gateway
  serves both, or the optimizer needs its own variables, which is a code
  change.
- The script refuses to run step 2 without `SWEEP_APPROVED=yes`.
- An empty balance ends the sweep at the first campaign that gets `HTTP 402`.

## Proposed settings, and why

These are proposals. Change a default through the script's environment
variables, and write the change next to the result.

| Setting | Default | Why |
|---|---|---|
| Machine | The Mac the earlier sweeps ran on, with `sample` for profiling | Results compare to the earlier sweeps only on the same kind of machine. A Linux machine with `perf` works, and its result then stands on its own. |
| Quiet bar | 1-minute load below 3 (`MAX_LOAD`) | Foreign load has produced false accepts and false rejects. The script waits up to an hour, then runs and logs `NOT QUIET`; read those lines before trusting the result. |
| Campaign bounds | `max_duration` 40m, 6 candidates, stop after 3 failures in a row | Half the manifests' 90m and 12. A sweep wants a verdict per target, not the best patch per target. These are the only manifest fields the script changes. |
| Verification | `verify` on every accepted attempt, 60 pairs | The default, and the count that caught the miller false accept (`DefaultVerifyPairs`). |
| Null candidates | 7 per target, 70 in all | The same total as the claim being re-checked. With no false accept in 70, the 95% upper bound on the false-accept rate is about 4%. 7 is small per target and enough in sum. |
| Stability bar | Two clean sweeps in a row on the same revision, on the same machine | As used before. One sweep cannot show that a clean result repeats. |

A sweep is **clean** when all of these hold:

1. Every verification of an accepted patch holds (`held: true`). The scorecard
   shows verified and held counts for each campaign; they must be equal, and
   every accepted patch must be verified.
2. No null candidate is accepted: the null table's `false accepts` column is 0.
3. No campaign ends on a harness fault: a crash, a `failed` stop reason, or
   verdicts in the `harness` class that are not an out-of-credit stop.
4. No campaign or verification ran loaded, as the script's log shows.

Inconclusive campaigns, and rejected candidates, are expected and do not break
a sweep. Most campaigns end inconclusive by design.

## Steps

```sh
export SWEEP_DIR=$PWD/heldout-sweep      # everything lands here

# 1. Build the binary under test from a committed revision.
scripts/heldout-sweep.sh build <rev>

# 2. One bounded live campaign per target. SPENDS MONEY.
export OPENROUTER_API_KEY=...            # and OPENROUTER_BASE_URL, GOTORQUE_MODEL_OPTIMIZER
SWEEP_APPROVED=yes scripts/heldout-sweep.sh campaigns

# 3. Measure every accepted patch again.
scripts/heldout-sweep.sh verify

# 4. Null candidates: patches that change nothing, so any accept is false.
scripts/heldout-sweep.sh null

# 5. Sum the verdicts.
scripts/heldout-sweep.sh scorecard
```

Each step skips what already exists, so an interrupted sweep continues where it
stopped. For a second sweep, use a new `SWEEP_DIR` and the same `<rev>`.

What the script leaves in `SWEEP_DIR`:

- `version.txt`: the binary's revision. Every campaign also records its build,
  and the scorecard warns when campaigns mixed builds or ran a dirty one.
- `settings.txt`: the optimizer model, the endpoint and the bounds.
- `revisions.tsv`: the commit of each target repository that was cloned. The
  manifests name a repository, not a commit, so this is what makes the sweep
  repeatable.
- `campaigns/<target>`, `null/<target>`: the campaign directories, with
  `<dir>.log` and `<dir>.verify.log` beside them.
- `scorecard.md`: the verdict table, with the false-accept and false-reject
  counts from the null table.

## Reporting

Update the README's Status section from `scorecard.md` and the logs. Name:

- the gotorque revision (`version.txt`),
- the date, the machine and the optimizer model (`settings.txt`),
- how many sweeps ran, how many accepted patches were verified and held, and
  how many null candidates ran with how many false accepts and rejects.

If a sweep is not clean, say that the claim no longer holds, with the reason,
instead of keeping the old sentence. A failure goes to an issue first; it is
not fixed by changing a held-out manifest.
