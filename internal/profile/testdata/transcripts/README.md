# Recorded sampler transcripts

Each `NAME.json` is a `profile.Transcript`; `report_file` names the sampler report
beside it. They are what the sampler adapters returned, recorded from real runs, so the
classifier is tested against output the tools actually print (a hand-written report once
diverged from reality; see `ParseMacOSSample`).

| Transcript | Recorded from |
| --- | --- |
| `macos-busy` | `/usr/bin/sample` on a Go program sorting in a loop (CPU bound) |
| `macos-sleeping` | `/usr/bin/sample` on a Go program parked in `time.Sleep` |
| `macos-pup-idle` | the discovery sample of the first pup campaign: 35 samples, all waiting (issue 79) |
| `macos-exited-process` | `sample` pointed at a pid that had already exited: exit 255 |
| `macos-cannot-examine` | the stderr a gron/s2c campaign logged when `sample` lost a target while attaching (issue 81) |

No Linux `perf script` transcript is recorded: the host these were taken on is macOS.
