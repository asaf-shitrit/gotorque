# 0019. Jev picks the fix within a cause, and code overrules unbuffered-I/O flags on in-memory writers

- Status: accepted (opt-in with `--analyst jev`)
- Date: 2026-09-24

## Context

A target's remedy (ADR 0013) is one sentence per cause, and for most causes it names several
mechanisms: "reuse a buffer, drop a conversion, or keep the value on the stack". The optimizer
picks among them without seeing which the function needs.

Separately, Jev reads a function's source alone, so it cannot tell a write to a `*bytes.Buffer`
field from a write to a file. On gojq it flagged `(*encoder).writeByte`, which appends to an
in-memory buffer, for unbuffered I/O at +2.9 sd, and two of a campaign's three candidates went to
it.

## Decision

**Fix kinds.** For a site whose flagged causes include allocation, fast path or string building,
the analyst asks nine fix-kind questions in one more request over the same state. Each kind is
scored against its own baseline, as causes are. The leading kind is used only when its z is at
least 0 and it leads the cause's next kind by `KindGate` (0.25 sd). The target then carries
`fix_kind` and that kind's remedy; otherwise the generic remedy stands. A failed request changes
nothing. The redundant-work pair is asked, because the baseline was measured with all nine in one
request, but never chosen. Unbuffered I/O, preallocation and superlinear work get no kinds: every
benchmark fix of the first two used one mechanism, and superlinear has five cases. The drop-fmt kind
gets its own shape rule (ADR 0017): it removes a `fmt.` call or adds `strconv.`, and need not add
a builder.

**In-memory writers.** Before targets are built, code drops an unbuffered-I/O flag from a function
whose every read and write provably goes to a `bytes.Buffer`, `strings.Builder`, reader or `bufio`
value, resolved from the receiver's struct fields, the parameters and local declarations. A
destination it cannot resolve counts as real I/O. The drop is recorded under `overruled` in the
`cause_analysis` event.

## Evidence

Fix kinds, over the 93-fix benchmark's labelled cases (186 requests, same corpus and state as the
cause baseline):

| Cause | z-ranked top kind right | Always guessing the commonest kind | Behind the gate: right / chosen |
| --- | --- | --- | --- |
| Allocation | 16/21 | 9/21 | 14/18 |
| Fast path | 8/9 | 6/9 | 8/8 |
| String building | 8/12 | 8/12 | 7/8 |
| Redundant work (not shipped) | 7/12 | 6/12 | 5/8 |

The questions were written after reading those fixes and scored on them, so these are upper
bounds until a live campaign or a held-out set confirms them. Each labelled kind's z fell after
the real fix was applied, in every cause, so the questions respond to the mechanism rather than to
the function.

In-memory writers: on the gojq encoder the check drops the flag from `writeByte`, whose writes all
go to its `*bytes.Buffer` field, and keeps it on `printValues` and `flush`, which write to an
`io.Writer`.

## Consequences

A site with a kind-bearing cause costs one more Jev request. A wrong kind narrows the optimizer to
the wrong mechanism, and that candidate most likely measures inconclusive; the gate trades
coverage for precision for that reason. The in-memory rule knows only the types it lists, and a
buffer reached through anything but a field, parameter or local declaration keeps Jev's flag.
