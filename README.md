# NEAR E2EE Proxy

A small self-hosted Go service that exposes **OpenAI Chat Completions** and uses
NEAR AI's official SDK for verified, end-to-end encrypted inference.

Supports text JSON, incremental encrypted SSE, native function calls and encrypted
tool-result continuation, with hardened Linux AMD64/ARM64 container packaging and
GitHub validation CI, and guarded development and versioned image publication to
GHCR. Responses is not currently included.

The local host is trusted. The proxy also trusts NEAR's deployment and
update controls. It verifies CPU/GPU evidence and model-response signatures, but
does not independently audit the full running deployment or its network policy.
Read [the trust boundary](docs/SECURITY.md) before using sensitive workloads.

## Start locally

Use [mise](https://mise.jdx.dev/) for Go; no Homebrew or system-package changes:

```sh
mise trust mise.toml
mise --locked install
mise run build
```

Create `.env` from [.env.example](.env.example) and set `NEAR_AI_API_KEY` and a
separate `PROXY_API_KEY`. Generate a proxy token with:

```sh
./bin/near-e2ee-proxy -generate-key
```

Paste the generated token into `PROXY_API_KEY` in your local `.env`, then start:

```sh
mise run dev
```

Mise loads `.env` for `dev`, `live` and `benchmark`, with output redaction enabled.
The application reads credentials only from environment variables; it has no
dotenv parser or credential-file support.

The service listens on `127.0.0.1:8080`. Stop it with Ctrl-C. `.env` and `bin/`
are ignored by Git. Starting the service does not send inference.

In a second terminal, set `OPENAI_API_KEY` to the same value as `PROXY_API_KEY`:

```sh
export OPENAI_BASE_URL=http://127.0.0.1:8080/v1
export OPENAI_API_KEY="your-proxy-token"

curl --fail-with-body "$OPENAI_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "Qwen/Qwen3.8-27B",
    "messages": [{"role": "user", "content": "Reply with the single word OK."}],
    "max_completion_tokens": 64,
    "chat_template_kwargs": {"enable_thinking": false}
  }'
```

For another OpenAI-compatible **Chat Completions** client, configure the same base
URL, the proxy token as its API key, and `Qwen/Qwen3.8-27B` as the model. Keep its
fallback providers configured deliberately. Actual LiteLLM/Hermes/OpenCode/Orion
versions have not been tested; see [the supported contract](docs/COMPATIBILITY.md).
Codex needs Responses support or an existing external adapter.

Choose a model in each request's `model` field using its canonical NEAR ID. It
does not need to appear in the proxy's configured discovery list. The selected
model must pass CPU/GPU verification and support the encrypted inference path;
unavailable or non-confidential models fail without a plaintext fallback.

For SSE, add `"stream": true` and optionally
`"stream_options": {"include_usage": true}` to the body, and use `curl -N`.
**Text streams as it arrives, provisionally.** Pre-request hardware verification
and input encryption stay mandatory. Tool calls, finish reasons, usage and `[DONE]`
wait for the final model signature. A failed check emits a stream error without a
successful completion marker; text already consumed cannot be retracted. JSON
responses remain fully verified before delivery. Cold attestation still delays
the first request; allow up to three minutes overall. See [streaming security](docs/SECURITY.md#streaming-tradeoff).

## Containers

Build locally with `docker build -t near-e2ee-proxy:local .`, supply separate
`NEAR_AI_API_KEY` and `PROXY_API_KEY` process environment variables, then run
`docker compose --env-file /dev/null up --build -d`. The example publishes only
`127.0.0.1:8080` and runs non-root with a read-only filesystem and no capabilities.
Starting the container makes no inference request.

The image listens on `0.0.0.0:8080` inside its network namespace; the standalone
binary still defaults to `127.0.0.1:8080`. Its shell-free `-healthcheck` command
probes only local `/healthz`, without credentials or NEAR access. Readiness is a
separate authenticated check. See [container operations and CI](docs/CONTAINERS.md)
for build/test commands, key setup, shutdown behavior and deliberate remote access.
Validated development images are published as
`ghcr.io/yaasha/near-e2ee-proxy:main` and `:sha-<full-commit>` for Linux AMD64/ARM64.
Version tags publish preserved full-version images; stable releases can advance
minor aliases and `latest` without moving them backwards. See the
[release procedure](docs/CONTAINERS.md#versioned-releases). Pin an exact digest
for deployment; publication does not update running services.
See [distribution and deliberate updates](docs/CONTAINERS.md#development-images-on-ghcr),
including the first-publication visibility requirement and anonymous-pull checks.

## Configuration

| Setting | Meaning |
| --- | --- |
| `NEAR_AI_API_KEY` | Outbound NEAR Cloud credential |
| `PROXY_API_KEY` | Inbound shared token, 32–256 non-whitespace ASCII characters |
| `LISTEN_ADDR` | Default `127.0.0.1:8080` for the standalone binary; `0.0.0.0:8080` in the image |
| `MODELS` | Models advertised by `/v1/models` and probed by `/readyz`; default `Qwen/Qwen3.8-27B`; at most eight |

`MODELS` is a discovery/readiness list, not an allowlist. Authenticated requests
can select other canonical model IDs, each verified before inference. Readiness
checks the configured entries only; it does not guarantee availability of every
other model.

Mise's development tasks load all settings in `.env`, so it can also contain
`LISTEN_ADDR`, `MODELS` or the proxy-key settings. See [mise's task-specific
environment support](https://mise.jdx.dev/environments/#task-specific-variables).
Offline tasks do not load this file. The standalone binary reads only its process
environment; deployments can supply these variables without mise. There are no
application dotenv flags, credential-file settings or custom parsing rules.

To use other machines, set `LISTEN_ADDR` deliberately and protect the
client-to-proxy connection with HTTPS or an encrypted tunnel/VPN. A bearer token
authenticates clients; it does not encrypt their prompts over plain HTTP.
Any local TLS-terminating reverse proxy must also be trusted and avoid body logs.

The NEAR upstream is fixed to `https://cloud-api.near.ai/v1`. There is no upstream
URL override, plaintext mode, verification-disable switch, provider fallback or
automatic inference retry. All routes except process liveness require proxy auth.

## Development and validation

| Command | Purpose |
| --- | --- |
| `mise run dev` | Mise loads `.env`, then runs the proxy from source |
| `mise run build` | Build the proxy into `bin/` |
| `mise run fmt` | Format Go source in place |
| `mise run format-check` | Check formatting without changing files |
| `mise run vet` | Check for common Go mistakes |
| `mise run test` | Offline tests with the race detector |
| `mise run smoke` | Check CLI startup and an HTTP listener with a synthetic backend |
| `mise run check` | Formatting, vet, offline tests, build and smoke checks |
| `mise run lint` | Pinned workflow and Dockerfile lint checks; no source rewriting |
| `mise run container-test` | Offline built-container and Compose regressions; requires Docker |
| `mise run distribution-test` | Offline publication gates and registry-inspection fixtures |
| `mise run live` | Four short paid requests: JSON, SSE, tool call and tool result |
| `mise run benchmark` | Ten bounded paid requests comparing proxy/API timing; [methodology](docs/BENCHMARKING.md) |

Only `dev`, `live` and `benchmark` load `.env`.
Offline checks use synthetic fixtures and do not load credentials from files.
Live tests require opt-in, cap each request at 128 output tokens, and never print
keys, prompts or answers.
They do not retry inference. They are excluded from ordinary checks.

## Reading the Go code

| File | Responsibility |
| --- | --- |
| [main.go](cmd/near-e2ee-proxy/main.go) | Flags, startup, HTTP deadlines and graceful shutdown |
| [config.go](internal/proxy/config.go) | Environment settings and credential validation |
| [server.go](internal/proxy/server.go) | Authentication, routes, limits and safe errors |
| [request.go](internal/proxy/request.go) | Supported request types and validation |
| [json.go](internal/proxy/json.go) | Exact field names, duplicate keys and nesting limits |
| [response.go](internal/proxy/response.go) | Complete response and usage validation |
| [stream.go](internal/proxy/stream.go) | Provisional text, buffered tools and verified stream completion |
| [client.go](internal/near/client.go) | Mandatory SDK verification, E2EE and response receipts |
| [stream.go](internal/near/stream.go) | Native SDK encrypted streaming and bounded SSE parsing |

`package main` defines a runnable command and `func main()` starts it. `internal/`
contains application packages that other repositories cannot import. Contexts
carry cancellation/deadlines; the buffered channel in the HTTP server limits
concurrent work. Tests live beside the code in `*_test.go` files. `go test ./...`
means all packages in the module.

Go and the validation linters are pinned in `mise.toml`/`mise.lock`. Builds reject
compiler drift from those pins and `go.mod`. The only direct application dependency is
NEAR's SDK at `v0.2.0-rc.1` (a release candidate); `go.mod` and `go.sum` record its
transitive dependencies. Their verification machinery is larger than this proxy.

- [Contributing](CONTRIBUTING.md) and [agent instructions](AGENTS.md)
- [Compatibility and limits](docs/COMPATIBILITY.md)
- [Security assumptions](docs/SECURITY.md)
- [Benchmarking](docs/BENCHMARKING.md)

Licensed under the [MIT License](LICENSE).
