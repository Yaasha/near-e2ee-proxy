# Benchmarking

```sh
mise run benchmark
```

This opt-in test compares the real proxy HTTP handler and NEAR's ordinary API,
using the same model and synthetic prompt. Mise loads a local test key from `.env`.
It sends exactly ten sequential requests, each capped at 256 output tokens, and
writes metadata-only results to `.cache/benchmarks/latest.json`. Results are local
artifacts and are ignored by Git. The benchmark may incur API charges.

Four nonstreaming pairs alternate request order; one streaming pair measures time
to first nonempty content. The first pair uses fresh client sessions, followed by
warm sessions. Temperature, seed, reasoning and output limits are held constant.
The direct API baseline exists only in this test; it is never a production fallback.

Recorded phases:

- **Attestation:** SDK gateway/model evidence and hardware verification. Warm
  requests may reuse the configured five-minute evidence cache.
- **Inference exchange:** transport, queueing, prefill, generation and SDK
  encryption/decryption. Streaming also includes local validation and flushing.
  This is not a measurement of pure GPU generation speed.
- **Signature verification:** receipt retrieval and exact-wire verification,
  including bounded backoff if needed. Inference is never retried.
- **First content and total:** times observed by the HTTP client. Text is
  provisional; successful proxy completion includes the final model receipt.

Reports contain durations, token counts and fixed metadata, never prompts,
responses, keys or tool data. Test instrumentation does not change production
verification or add production logging. Ordinary checks do not execute benchmarks.

Compare repeated runs under similar conditions. Provider load, serving instances,
context length and cache behavior can differ between paths and runs. A small
sample cannot isolate encryption overhead or establish a throughput guarantee.
