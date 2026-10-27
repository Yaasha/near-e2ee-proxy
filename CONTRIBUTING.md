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
```

The check command runs formatting validation, `go vet`, race-enabled offline tests,
a build and an HTTP smoke test. It needs no API keys and makes no paid requests.
Use the pinned compiler and dependency versions. Keep source comments helpful to
someone learning Go, especially around validation and verification boundaries.

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
