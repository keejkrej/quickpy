# QuickPy soak/resource evidence

`TestSoak` is the opt-in resource/soak test. It is skipped unless
`QUICKPY_SOAK` is set, and it still skips when `QUICKPY_TEST_ARTIFACT` is
unset. It is a bounded, repeatable workload, not an endless stress test.

## Command

```sh
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm \
QUICKPY_SOAK=1 \
go test -count=1 -run TestSoak -v ./integration
```

## Workload

- 20 create/close cycles. Each cycle creates a fresh `quickpy.Runtime`, runs a
  batch, and closes it.
- Per cycle:
  - 40 sequential executions with a print, a host tool call, and a state write.
  - 10 concurrent executions (distinct globals) with the same print/tool/state
    shape.
  - 1 recursion/allocation execution: recursion depth 50 and a 1000-element
    list, then a state write.
  - 1 cancellation execution: a noncooperative handler is outstanding, the
    execution is cancelled, the handler is released after cancellation, and a
    later `Wait` must still see the same terminal `Cancelled` result with no
    state.
  - 1 healthy execution after the cancellation to prove capacity was released.
- Total: 20 × 53 = **1060 guest executions** (planned 1040; the extra 20 are
  the post-cancellation healthy executions).

The test asserts:

- every execution completes with the expected status, output, and state;
- the total execution count is at least 1000;
- the error count is zero;
- the cancelled execution commits no state, and a late handler return does not
  change the terminal result;
- an immediate execution after cancellation succeeds (no leaked admitted
  capacity).

## Metrics method

- **Artifact identity:** size and SHA-256 are read from the artifact file and
  logged. The test does not hard-code the hash.
- **Elapsed:** wall-clock time around the whole workload.
- **RSS:** `ps -o rss= -p <pid>` (the same read-only OS method used by
  `cmd/quickpy-spike`). This is a coarse process-level sample, not a
  per-execution memory measurement.
- **Goroutines:** `runtime.NumGoroutine()` after each batch, with a bounded
  quiescent retry. The test does not assert an exact count.
- **Allocations:** `runtime.ReadMemStats` after `runtime.GC()` per batch
  (`HeapAlloc`, `TotalAlloc`).
- **Growth check:** the test fails only when a metric increases strictly on
  every batch and the total increase exceeds a generous threshold
  (goroutines +64, RSS +256 MiB, heap +64 MiB). Equal or noisy samples are
  informational.

## Observed run (2026-11-18, ABI 4 final)

Artifact: **236,119 bytes**, SHA-256
`171502b0e072d03402672f75ea7c35e651341518d0f452c533995c7106372184`, ABI 4.

Result: **PASS**, 1060 executions in **32.51 s**, errors **0**.

| Sample | Value |
|---|---|
| RSS before | 8,016 KiB |
| RSS after | 372,304 KiB |
| RSS quiescent | 372,304 KiB |
| Goroutines before | 2 |
| Goroutines after (quiescent retry) | 2 |
| HeapAlloc before | 337,720 B |
| HeapAlloc after | 8,501,552 B |
| TotalAlloc delta | 44,740,574,920 B |

Per-batch trend (selected columns; full log is emitted by `-v`):

| Batch | Exec | Elapsed | RSS KiB | Goroutines | HeapAlloc | Errors |
|---:|---:|---:|---:|---:|---:|---:|
| 00 | 53 | 1.480 s | 333,664 | 2 | 853,016 | 0 |
| 01 | 53 | 1.416 s | 334,144 | 2 | 1,283,296 | 0 |
| 05 | 53 | 1.913 s | 332,592 | 2 | 2,984,016 | 0 |
| 10 | 53 | 1.480 s | 362,768 | 2 | 4,978,616 | 0 |
| 15 | 53 | 1.496 s | 343,840 | 2 | 6,935,200 | 0 |
| 19 | 53 | 1.710 s | 372,304 | 2 | 8,511,008 | 0 |

Diff after 20 batches:

- RSS: +38,640 KiB (~37.7 MiB) from batch 00 to batch 19. The series is not
  strictly monotonic (several batches are lower), so the coarse monotonic
  check did not fire. This is a process-level high-water observation, not a
  memory-bound proof.
- HeapAlloc after GC: +7,657,992 B (~7.3 MiB) from batch 00 to batch 19. The
  series is monotonic but well below the 64 MiB failure threshold. It is
  recorded as a trend, not as a claim that memory is bounded.
- Goroutines: steady at 2 per batch and 2 after the quiescent retry.
- Errors: 0 in every batch.

The soak workload is unchanged: it does not emit console or image output, so it
stays bounded and does not add heavy image buffers. Structured console/image
output is covered by the dedicated integration tests in `output_test.go`.

## Historical ABI 3 run (2026-11-18)

The last ABI 3 run used the 227,717-byte artifact with SHA-256
`a20ca657e77364104902dff759c8be42c964e2cd9fe8f21c97102a606c11a4fc`. Result:
**PASS**, 1060 executions in **25.90 s**, errors **0**.

| Sample | Value |
|---|---|
| RSS before | 8,016 KiB |
| RSS after | 357,520 KiB |
| RSS quiescent | 357,520 KiB |
| Goroutines before | 2 |
| Goroutines after (quiescent retry) | 2 |
| HeapAlloc before | 337,512 B |
| HeapAlloc after | 8,207,776 B |
| TotalAlloc delta | 44,701,839,696 B |

Per-batch trend (selected columns; full log is emitted by `-v`):

| Batch | Exec | Elapsed | RSS KiB | Goroutines | HeapAlloc | Errors |
|---:|---:|---:|---:|---:|---:|---:|
| 00 | 53 | 1.292 s | 341,856 | 2 | 836,496 | 0 |
| 01 | 53 | 1.331 s | 343,744 | 2 | 1,262,456 | 0 |
| 05 | 53 | 1.256 s | 355,888 | 2 | 2,850,792 | 0 |
| 10 | 53 | 1.282 s | 357,120 | 2 | 4,788,720 | 0 |
| 15 | 53 | 1.306 s | 357,392 | 2 | 6,676,376 | 0 |
| 19 | 53 | 1.232 s | 357,520 | 2 | 8,216,520 | 0 |

Diff after 20 batches:

- RSS: +15,664 KiB (~15.3 MiB) from batch 00 to batch 19. The series is not
  strictly monotonic (several batches are equal), so the coarse monotonic
  check did not fire. This is a process-level high-water observation, not a
  memory-bound proof.
- HeapAlloc after GC: +7,380,024 B (~7.0 MiB) from batch 00 to batch 19. The
  series is monotonic but well below the 64 MiB failure threshold. It is
  recorded as a trend, not as a claim that memory is bounded.
- Goroutines: steady at 2 per batch and 2 after the quiescent retry.
- Errors: 0 in every batch.

An earlier run of the same workload (before the final host identity fix)
measured RSS 7,968 KiB before and 388,624 KiB after, with the same 1060
executions and zero errors. The RSS delta varies with the host process and
allocator state; the test records the current run rather than asserting a
fixed number.

## Interpretation and limits

- The workload is deliberately moderate so it can be repeated in a few
  minutes. It is not a long-running leak detector.
- A single RSS sample is **not** evidence that memory is bounded. The test
  records before/after/quiescent samples and a per-batch trend; it only fails
  on a clearly monotonic, large increase across comparable batches.
- The guest heap is 4 MiB and linear memory is capped at 64 MiB. The process
  RSS includes wazero, the Go runtime, and test bookkeeping, so it is expected
  to be much larger than the guest heap.
- The cancellation case uses a noncooperative handler that ignores its context.
  The test releases it after cancellation and verifies that the late return
  does not commit state or change the terminal result. It does not claim that
  the handler is forcibly stopped.
- The test does not use `t.Parallel` and does not write to process stdout, so
  it does not depend on global runtime state or interleave output with other
  tests.
