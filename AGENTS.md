# Contributor and agent guidance

Read `README.md`, `docs/COMPATIBILITY.md` and `docs/SECURITY.md` before changing
protocol behavior. Use `mise.toml` and `mise.lock` for tools; do not modify system
packages or global tool versions. Application dependencies belong in `go.mod`.

## Development

- Run `mise run fmt` and `mise run check` before submitting changes. Add meaningful
  tests alongside functionality, including failure cases at security boundaries.
- Prefer the Go standard library, small functions and ordinary control flow.
  Explain security decisions in comments; avoid frameworks and custom crypto.
- Pin the NEAR SDK and review dependency/policy changes. Check licenses before
  copying code from other projects. Do not automatically upgrade dependencies.
- Keep manual test pages, diagnostic spikes and generated reports out of the
  repository. Maintain repeatable regression tests alongside the source instead.
- Keep changes scoped. Responses and stable semantic-version releases are not
  currently implemented; do not add them as incidental cleanup or claim they are available.

## Credentials and live testing

Credentials come only from environment variables. Never open, print or commit
real `.env` files or credential values.
Mise loads `.env` only for `dev`, `live` and `benchmark`, with output redaction
enabled. Do not add application dotenv parsing.
Ordinary checks must not load credentials or make paid requests.

Live tests and benchmarks require explicit authorization and a limited test key.
Use synthetic prompts, short output limits and fixed request counts. Never print
keys, prompts, answers, tool data or raw SDK error causes. Live inference tests
and benchmarks may incur charges. Benchmark results belong in the ignored
`.cache/` directory, not in source control.

## Security invariants

- The local proxy host and NEAR's deployment/update controls are trusted. Hardware
  verification does not independently audit the running software or remote egress.
- Every inference route uses the official SDK's model-key E2EE. Never add plaintext
  passthrough, provider fallback or a production verification-disable switch.
- Authenticate before parsing bodies, contacting NEAR or doing expensive work.
  Client and NEAR bearer secrets are separate; never forward caller headers.
- Verify CPU/GPU evidence and key binding before encryption. Check every returned
  model candidate, enforce canonical IDs and keep gateway TLS binding enabled.
- Recursively validate accepted fields against SDK encryption coverage. Reject
  unknown fields, duplicate keys and unsupported content instead of forwarding it.
- Require a `provider_tee` receipt over the SDK's exact encrypted wire bytes before
  releasing JSON answers, tool calls or successful stream completion. Gateway
  receipts cannot substitute for model receipts.
- Streaming text/reasoning/refusal is explicitly provisional. Withhold tool calls,
  finish reasons, usage and `[DONE]` until verification succeeds. On failure, emit
  a safe error without successful completion. Document that text cannot be retracted.
- Never log bodies, keys, tool data, headers or raw upstream errors. Keep no
  telemetry, persistent conversations or debug dumps.
- Bound requests, responses, concurrency, caches and deadlines. Honor cancellation;
  never retry inference. Only transient receipt retrieval may retry within limits.
- Keep upstream destinations fixed and HTTPS-only; reject redirects and caller
  encryption/routing headers. Do not add authentication bypasses or browser CORS
  exceptions for temporary testing tools.

Update documentation and regression tests when supported behavior changes. Do not
claim an audit, independent runtime approval or untested client compatibility.

## Agent skills

### Issue tracker

Track issues and specs in GitHub Issues for `Yaasha/near-e2ee-proxy`.
Before working with tickets, read `docs/agents/issue-tracker.md`.

### Triage labels

Use the five default triage labels.
Before triaging issues, read `docs/agents/triage-labels.md`.

### Domain docs

Use a single-context layout: root `GLOSSARY.md` and `docs/adr/`.
Before exploring the codebase, read `docs/agents/domain.md`.
