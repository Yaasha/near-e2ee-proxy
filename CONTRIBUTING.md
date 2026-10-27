# Contributing

Small, focused pull requests are welcome. For protocol changes, describe the
client behavior you need and check [the supported contract](docs/COMPATIBILITY.md)
and [security boundary](docs/SECURITY.md) first.

## Set up and check changes

```sh
mise trust mise.toml
mise --locked install
mise run fmt
mise run check
mise run lint
```

The check command runs formatting validation, `go vet`, race-enabled offline tests,
a build and an HTTP smoke test. It needs no API keys and makes no paid requests.
Use the pinned compiler and dependency versions. Keep source comments helpful to
someone learning Go, especially around validation and verification boundaries.

GitHub Actions runs these validation gates on main pushes and opened, updated or
reopened PRs. CI checks formatting without changing submitted files. Separate
native AMD64/ARM64 jobs build and run the container regressions. Validation jobs
have read-only repository permissions and receive no application credentials.
PRs never log into GHCR or publish images. Trusted current-main push and manual
runs publish development images only after all validation succeeds; only the
serialized publication job has package-write permission. Anonymous pull checks
then run separately for both architectures. See
[distribution](docs/CONTAINERS.md#development-images-on-ghcr).

For container changes, run `CONTAINER_ARCH=amd64 mise run container-test` and
`CONTAINER_ARCH=arm64 mise run container-test`. Docker must support running the
selected Linux architecture, natively or through explicit emulation (Docker
Desktop includes it). These tests create temporary synthetic contexts and use
synthetic environment credentials, not real env files or inference. See
[container validation](docs/CONTAINERS.md#validation-and-ci) for coverage and pins.

Add tests for changed behavior, including relevant rejection paths. Use synthetic
fixtures; never put real prompts, tool results, credentials or captured SDK errors
in tests, issues or pull requests. Do not commit `.env`, credentials, binaries,
coverage output or benchmark captures.

## Optional live validation

Copy `.env.example` to your local `.env` and provide a limited NEAR test key.
`mise run live` makes four synthetic HTTP requests covering JSON, streaming,
function calls and tool-result continuation, each capped at 128 output tokens.
It does not retry inference and may incur charges. It is separate from ordinary
checks. See [benchmarking](docs/BENCHMARKING.md) for performance comparisons.

Include the relevant check results in your pull request. Keep security settings
enabled when validating interoperability; unsupported inputs should fail clearly
until their encryption coverage is reviewed and tested.
