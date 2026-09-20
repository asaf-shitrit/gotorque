# 0005. The behavior gate subtracts pre-existing failures and refuses unattributable baselines

- Status: accepted
- Date: 2026-09-20

## Context

The gate rejected every candidate of a live gojq campaign within twenty minutes for a failure no
patch could have caused: the target's own suite already fails on the unpatched revision under Go
1.27, whose `encoding/json` reports "unexpected end of JSON input" where the test still expects
"unexpected EOF". Both attempted candidates built cleanly and neither produced a single measurement
artifact, so the gate was attributing the operator's toolchain to the model.

## Decision

Run `go test -json` once on the unpatched revision during baseline discovery, record the failing
tests, and reject a candidate only for failures absent from that set. A baseline whose tests never
ran (build or setup failure) stops the campaign instead.

## Consequences

The gate attributes breakage to patches rather than to the environment. A target with a
pre-existing red suite can still be optimized, and its report states what the gate could verify.
A candidate that breaks a test the baseline passed is still rejected, and the report names it.

## Alternatives considered

Require exit 0 on the patched tree (rejected: on a drifted toolchain it rejects everything). Compare
failing packages rather than tests (rejected: too coarse, a break inside an already-red package would
pass).
