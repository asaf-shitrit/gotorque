# 0032. Measured unbuffered writes raise a code-derived target

- Status: accepted
- Date: 2026-09-28

## Context

Writing each record with its own system call is the most reliable win on record: gron's output
loop (−10.8%, then −15.1%) and gojq's `printValues` (−6.9%) were both fixed by a `bufio.Writer`.
Jev finds the pattern when it is visible in the function it is shown, as it was in gron's. On
fzf's filter mode it was not. The target sample put fzf's second-largest weight on
`defaultOptions.func1`, the Printer closure `func(str string) { fmt.Println(str) }`, 95% of it
in `write`. Hot locations resolve a closure to its enclosing declaration, and the analyst sends
Jev the whole declaration because that is the unit its baseline was measured on (ADR 0012). The
enclosing declaration here is a 2.4 KB options constructor where the closure is one field
among about a hundred, and Jev put `unbuffered_io` at 0.04. The campaign spent its four attempts
elsewhere and accepted nothing.

Sending Jev the closure alone would put its answers outside the baseline, so z-scores would no
longer mean anything. The sample already holds better evidence than source text: whether the
time was spent in write system calls, and whether a `bufio` frame was on the way.

## Decision

`profile.UnbufferedWrites` computes, per own function, the fraction of its attributed samples
spent below `syscall.write` with no `bufio.` frame between it and the kernel. Orphaned `write`
samples (macOS `sample` cannot unwind across `asmcgocall`) are shared as attribution already
shares them. Discovery keeps each hot-list function at or above 0.5, with up to three callers,
in `State.DiscoveryUnbufferedWrites`. The analyst turns each into a target with cause
`unbuffered_writes`, placed first like `throwaway_result` (ADR 0027):

- Callers whose declarations fit the classification limit join as a function set. They are
  places the buffering may go, not required edits. The shape check asks for what Jev's
  `unbuffered_io` asks for: a `bufio` writer that is flushed.
- A Jev `unbuffered_io` target at the same location is dropped, so the remedy is not tried
  twice.
- Jev's state, questions and baseline are untouched.

## Evidence

Replayed over all 45 campaign sample reports on record, the functions
at or above 0.3 were exactly gron's `main.gron` (0.97), gojq's `printValues` (0.99) and
`(*encoder).flush` (0.98 to 1.00), and fzf's Printer closure (0.95). All but fzf's are known
fixes. No other function reached 0.3.

## Consequences

- A program that writes large chunks without `bufio` (an `io.Copy` to stdout) could fire. Its
  candidate would then measure no gain and come back inconclusive, costing one attempt.
- Only the target sample carries stacks, so a benchmark-only discovery raises nothing here.
