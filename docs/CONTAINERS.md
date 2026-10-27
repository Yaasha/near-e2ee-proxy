# Containers and validation CI

## Build and run

Docker Engine with Buildx and Compose v2, or Docker Desktop, is required. Build
the host's Linux architecture with:

```sh
docker build -t near-e2ee-proxy:local .
docker run --rm --network none near-e2ee-proxy:local -generate-key
```

The second command prints a new proxy token. Supply it and a separate NEAR key
through your process environment or deployment secret injector:

```sh
export NEAR_AI_API_KEY='your-limited-near-key'
export PROXY_API_KEY='your-separate-generated-proxy-token'
docker compose --env-file /dev/null up --build -d
docker compose --env-file /dev/null ps
```

The explicit null env file disables Compose's automatic `.env` loading. Compose
passes the two required settings into the process; the application continues to
read only environment variables. No credential is a build argument, image default
or file mount. Proxy tokens must be 32–256 non-whitespace ASCII characters and
differ from the nonempty, single-line ASCII NEAR token. Missing, malformed or equal
keys fail at startup with fixed messages. Each inference request selects its own
canonical model, which must pass the existing verification checks. `MODELS` only
customizes the discovery/readiness list when explicitly supplied through
`docker run --env MODELS` or a Compose override; it does not restrict requests.

The image runs UID/GID 65532 and listens on `0.0.0.0:8080` inside the container.
The standalone binary retains its loopback default. `LISTEN_ADDR` overrides still
work with `docker run --env LISTEN_ADDR=0.0.0.0:9090`; publish the corresponding
container port deliberately. The Compose example inherits the image's listener
default and the application's discovery/readiness default; only credentials are
required. It publishes `127.0.0.1:8080:8080`, uses a read-only root filesystem,
drops all capabilities, sets `no-new-privileges`, and bounds process count. It needs no
writable volume or temporary filesystem for startup and local HTTP operation.

For a direct run equivalent to the default Compose setup:

```sh
docker run -d --name near-e2ee-proxy \
  --restart unless-stopped --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true --pids-limit 128 --stop-timeout 15 \
  -p 127.0.0.1:8080:8080 --env NEAR_AI_API_KEY --env PROXY_API_KEY \
  near-e2ee-proxy:local
```

Use the existing OpenAI-compatible base URL `http://127.0.0.1:8080/v1` and proxy
token. To reach the service from another machine, deliberately arrange a trusted
HTTPS terminator or encrypted tunnel/VPN. Keep plaintext bearer traffic on the
trusted local segment, disable body/header logging at the terminator, and disable
reverse-proxy buffering for incremental SSE. A container port binding is not
transport encryption; the proxy host and TLS terminator remain trusted.

## Health, restarts and shutdown

The image health check executes `/near-e2ee-proxy -healthcheck` directly, without
a shell or a second utility. It makes only `GET /healthz` against `LISTEN_ADDR`,
mapping wildcard listeners to loopback. It accepts loopback IPs or `localhost`,
rejects remote destinations and redirects, ignores ambient HTTP proxy settings,
and uses a two-second deadline. Exit status 0 means HTTP 200; status 1 means an
invalid/unavailable listener, timeout or other HTTP status. It emits no output
and requires no credentials. Use wildcard or loopback listeners with this probe.

```sh
docker compose --env-file /dev/null exec proxy /near-e2ee-proxy -healthcheck
```

Docker probes every 30 seconds with a three-second execution limit, five-second
startup allowance and three retries. `/healthz` proves process HTTP liveness
only. Authenticated `GET /readyz` separately checks configured models' hardware
admission, can contact NEAR and verification services, and makes no inference
request. Liveness never establishes model readiness or verifies a reply.

`unless-stopped` restarts an exited service, including after daemon restarts
unless an operator stopped it. Docker does not restart a running container solely
because it becomes unhealthy. Monitor health and readiness deliberately.

The exec-form entry point makes the application PID 1 so SIGTERM reaches it
directly. The application stops accepting connections and allows up to ten
seconds for active work, then closes remaining connections. Compose allows 15
seconds before forced termination. Stop with:

```sh
docker compose --env-file /dev/null down
# For the direct docker run example:
docker stop --timeout 15 near-e2ee-proxy
docker rm near-e2ee-proxy
```

## Validation and CI

```sh
mise --locked install
mise run fmt
mise run check
mise run lint
CONTAINER_ARCH=amd64 mise run container-test
CONTAINER_ARCH=arm64 mise run container-test
```

Ordinary checks do not load `.env` or make paid requests. The container task
requires Docker and registry/module access to build; it uses synthetic credentials
and calls only liveness and unauthenticated protected routes. It also maps NEAR's
fixed hostname to container loopback as an egress guard. It never runs inference,
authenticates readiness, opens real env files or inspects secret mounts. Temporary
build contexts, exported runtime archives and synthetic exclusion sentinels are
removed by the test harness; test images, containers and networks are cleaned up.

The regressions verify the actual static ELF binary has no dynamic loader, parse
the runtime CA bundle, check timezone material and absence of shells/Go tooling,
test synthetic local-state exclusion and compiler drift rejection, exercise CLI/startup/credential rejection,
check real published loopback HTTP and alternate listener ports, test health-probe
success/failure, and require SIGTERM to exit cleanly within the 15-second grace
period. Compose configuration is checked without dotenv loading. Existing Go
tests retain the synthetic backend, real-listener HTTP, authentication,
encryption and receipt coverage; there is no production fake backend or bypass.

Both Linux architectures were built and run locally on Docker Desktop (ARM64
native, AMD64 emulated). This establishes static startup and local HTTP operation
with the pinned SDK and runtime, not live inference in the container. Existing
live-model results in the compatibility document are separate. No live inference
or paid request is part of these checks.

CI runs on main pushes and opened, synchronized, reopened or ready-for-review PRs.
The validation job runs `mise run lint` and the existing `mise run check`; it
reports formatting errors without rewriting source. Separate `ubuntu-24.04` and
`ubuntu-24.04-arm` jobs build and execute AMD64/ARM64 images on native runners,
rather than treating cross-compilation as runtime evidence. Jobs have bounded
execution times, `contents: read`, no application secrets, no package-write
permissions and no registry login/publication. PR code runs under `pull_request`,
without a privileged `pull_request_target` context. Caches are not shared by the
mise action. Automated GHCR publication remains follow-up work.

For an explicit single-platform local build, use:

```sh
docker buildx build --load --platform linux/amd64 -t near-e2ee-proxy:local .
# Or select linux/arm64. Run the matching container-test task afterward.
```

## Dependency pins

`mise.toml` and `mise.lock` pin Go 1.27.3, actionlint and hadolint, including linter
checksums for both Linux and macOS architectures. There is no new Go lint ruleset.
`GOTOOLCHAIN=local` prevents an implicit compiler download. The build/check tasks
and Docker build reject disagreement among the actual compiler, mise config,
lockfile and `go.mod`. The Docker build base supplies that exact compiler; it
does not install or select another version. Application/SDK dependencies remain
pinned in the unchanged `go.mod`/`go.sum`.

The multi-stage Dockerfile cross-compiles with `CGO_ENABLED=0` on the builder's
native architecture and ships only the executable on distroless static Debian 12
nonroot, which supplies CA certificates and timezone data. It contains no shell
or build tools. The build context allows only named manifests, the version-check
script and Go source in the production packages; tests and arbitrary local state
are excluded. Do not add credentials as image arguments, labels or build metadata.

The build and runtime multi-platform digests were resolved from the official
`golang:1.27.3-bookworm` and `gcr.io/distroless/static-debian12:nonroot` indexes on
2026-10-27. Dockerfile digests cover both supported architectures. Actions are
pinned to the resolved commits for `actions/checkout` v7.0.1 and `jdx/mise-action`
v5.2.4 after reviewing their manifests (both use Node 24); the mise bootstrap is
also version-pinned. Updating a pin requires reviewing the upstream change,
refreshing relevant lock checksums/digests and rerunning the matching checks.
These pins and tests do not constitute an independent security audit or runtime
approval of NEAR's deployment.
