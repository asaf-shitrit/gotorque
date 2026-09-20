# 0006. Model calls stream, and silence — not total duration — is the bound

- Status: accepted
- Date: 2026-09-20

## Context

ADK requests non-streaming responses, so the endpoint sent nothing until generation finished and a
whole-request timeout was the only available bound. That bound cannot tell a slow model from a dead
connection: every campaign round logged two to eight `context deadline exceeded` failures at exactly
the four-minute client timeout while legitimate role calls ran fifteen seconds to three minutes, and
each stalled attempt consumed its whole slot in the retry ladder. Streaming introduced a second
defect that a live call caught: ADK's final aggregated item repeats the answer and carries the
model's reasoning, so returning it verbatim produced `{"ok":true}{"ok":true}`.

## Decision

Stream every call, rebuild the single response a non-streaming call would have produced by
accumulating the non-thought deltas and holding the last item back, and fail a call that goes silent
for two minutes. The client carries no total timeout; the attempt bound (4 m) and the node deadline
(20 m) are sized so the ladder fits.

## Consequences

Stalls fail in two minutes and retry instead of consuming four. A producing call keeps its time.
Usage still arrives from the endpoint's last item, verified against the live endpoint.

## Alternatives considered

A longer whole-request timeout (rejected: waits longer for a stream that may never start).
No bound at all (rejected: one hung request consumes the node's whole deadline).
