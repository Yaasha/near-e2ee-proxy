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

CI runs on main and `v*` tag pushes, manual dispatch and opened, synchronized,
reopened or ready-for-review PRs.
The validation job runs `mise run lint` and the existing `mise run check`; it
reports formatting errors without rewriting source. Separate `ubuntu-24.04` and
`ubuntu-24.04-arm` jobs build and execute AMD64/ARM64 images on native runners,
rather than treating cross-compilation as runtime evidence. Jobs have bounded
execution times, `contents: read`, no application secrets, no package-write
permissions and no registry login/publication. PR code runs under `pull_request`,
without a privileged `pull_request_target` context. Caches are not shared by the
mise action. Successful trusted main runs can then publish as described below.

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

## Development images on GHCR

The CI workflow publishes `ghcr.io/yaasha/near-e2ee-proxy` for Linux AMD64 and
ARM64. `:main` follows successfully validated current main; `:sha-<full-commit>`
identifies the source commit. A rerun can produce a different artifact for the
same source (for example, creation timestamps or scanner output can change).
Use `@sha256:<index-digest>` to identify an exact image index and its attachments.
Stable and prerelease images follow the [versioned release procedure](#versioned-releases);
main builds never update `latest` or a stable minor alias.

The Go checks, pinned workflow/Dockerfile linters, and both native architecture
container jobs must all succeed before publication. The publication job checks
the repository, event, ref, checkout and current main revision before registry
login. PRs, forks and manual dispatches on other branches cannot publish. Version
tags use the additional source and preservation gates described below.
Only that job receives `packages: write`; login uses the automatically supplied
short-lived `GITHUB_TOKEN`, with no separate registry secret. No application key
or inference is involved. The build uses an immutable Git context at the validated
commit, with the same pinned Dockerfile/compiler/dependencies as the runtime tests.

Publication jobs share the `ghcr-publication` concurrency group and are never
cancelled by a newer run. Development builds, releases and manual recovery all
use this same lock across source checks, registry reads, builds and tag writes.
GitHub does not guarantee queued run order, so the command rechecks main
immediately before promoting the inspected digest to `:main`. A stale run fails
without moving that tag; if main advances during the build, a source tag may have
been published, but promotion is refused. A new commit arriving just after the
last check can temporarily leave main pointing to the prior validated revision;
the shared lock prevents an older writer from overwriting a newer publication.

Each platform has OCI title, description, source repository, full revision,
`sha-<commit>` development version (or complete release version), MIT license and
UTC creation-time labels. The source label
and workflow token associate the GHCR package with this repository. BuildKit
attaches maximum-detail SLSA v1 provenance and an SPDX SBOM per platform. Before
promotion, `internal/distribution` reads the actual registry configs and attachment
contents, checks both architectures and labels, matches provenance Git material
to the expected revision, and requires nonempty SPDX packages. A missing,
inconsistent or unreadable attachment fails the job. Using its automatically
supplied token, the command also reads GHCR's package metadata through `gh` and
requires public visibility and the correct repository association before promotion.
The job summary records the
published tags and exact index digest. These build records support source and
dependency inspection; they do not independently audit or approve NEAR's live
runtime, deployment controls or remote egress. The existing authentication,
model-key E2EE, model receipts and provisional-streaming tradeoff remain unchanged.

To build manually, open this repository's **Actions → CI → Run workflow**, select
`main`, and run without inputs. It uses all the same validation/publication gates.
For a CLI equivalent, use:

```sh
gh workflow run ci.yaml --repo Yaasha/near-e2ee-proxy --ref main
```

Branch dispatches outside main run validation but cannot publish. Tag dispatches
follow the release rules below. Review the
publication and both `public-pull` job summaries before treating a run as evidence.

## Versioned releases

Merge the intended source and release workflow into main first. Choose a fresh
`vMAJOR.MINOR.PATCH` tag, such as `v1.2.3`, or a prerelease such as
`v1.3.0-rc.1`. Tags use strict SemVer decimal components (no leading zeros);
prerelease identifiers contain ASCII letters, digits or hyphens, separated by
dots, and numeric identifiers have no leading zeros. The resulting image tag is
at most 128 characters. Build metadata (`+build`) is rejected because OCI tags
cannot represent it unchanged; it is never silently removed or rewritten.

Create and push the tag deliberately, substituting the reviewed commit:

```sh
git tag -a v1.2.3 <reviewed-main-commit> -m 'Release v1.2.3'
git push origin refs/tags/v1.2.3
# Prerelease example, as a separate release:
# git tag -a v1.3.0-rc.1 <reviewed-main-commit> -m 'Prerelease v1.3.0-rc.1'
# git push origin refs/tags/v1.3.0-rc.1
```

A tag push runs all ordinary checks and both native architecture container jobs.
The publisher checks the event repository, checkout revision, current remote tag
(peeled for annotated tags) and reachability of that exact commit from current
main. Malformed versions, moved tags, off-main commits, PR events and failed or
incomplete checks cannot publish. A GitHub Release page is optional: its creation
is not a second publication trigger. The workflow at the tagged commit must
include this release implementation.

| Source | Full image tag | Mutable aliases |
| --- | --- | --- |
| Main | `sha-<full-commit>` | `main`, only for validated current main |
| `v1.2.3` | `1.2.3`, preserved | `1.2` and `latest` when semantically newer |
| `v1.3.0-rc.1` | `1.3.0-rc.1`, preserved | None |

There are no major-only aliases. Numeric version order decides alias advancement,
not publication time or string order: `1.2.10` is newer than `1.2.9`. A late
`1.2.8` can publish its absent full version without replacing `1.2.10` at `1.2`;
a release from an older minor can advance its own minor without downgrading
`latest`. Prereleases never compete for stable aliases. Existing stable aliases
must reference a preserved stable full version, with matching platform metadata,
revision, provenance and SBOM; unknown or inconsistent targets block publication.

The entire publication job holds the same non-cancellable `ghcr-publication`
concurrency lock for development, release and recovery. It reads registry state
after acquiring the lock and rechecks the source and full version after the
candidate build. GitHub does not guarantee ordering across runs; the default
concurrency queue can replace a pending job with a newer pending job. A cancelled
pending run has made no registry write and can be rerun. Do not bypass the lock
with another workflow, a local push, tag deletion, or package-admin rewrites.
GHCR does not provide a transactional compare-and-swap for these tag writes;
preservation relies on trusted repository/package administrators and all writers
following this workflow. Protect release Git tags against movement/deletion with
repository rules as part of administration.

Before building a release, an authenticated GHCR lookup must positively establish
that its complete version is absent. Only HTTP 404 with `MANIFEST_UNKNOWN` counts.
Authentication errors, network errors, rate limits, redirects, malformed replies
and ambiguous not-found responses fail closed. An existing full version refuses
replacement, including a repeated trigger or ordinary manual retry; the job log
and summary explain the refusal.

New release builds first push `candidate-<run-id>-<attempt>`, with the release
version in OCI labels. Actual registry inspection must validate Linux AMD64/ARM64,
source revision, complete version, licenses, timestamps, SLSA v1 provenance and
SPDX SBOM attachments before assigning the full-version tag. Alias decisions are
computed before any release-tag write. Tags are then written full version first,
minor alias next and `latest` last, each checked against the inspected digest.
The candidate may remain after failure; it is not an operator release alias.
Retain published release digests and their attachments; do not clean up candidate
manifests by deleting a digest shared with a preserved version.

### Manual release operation and recovery

To validate and publish an absent version manually, dispatch **Actions → CI →
Run workflow** on its existing `v` tag with `recover_digest` empty. Equivalently:

```sh
gh workflow run ci.yaml --repo Yaasha/near-e2ee-proxy --ref v1.2.3
```

Manual operation runs the same source, validation, registry and alias gates. It
cannot publish a feature branch, rebuild an existing full version or substitute
another revision in its labels. Keep main dispatches unchanged, without inputs.

If a run fails before the full version is written, fix the cause and rerun all
jobs or the failed publication job. It rechecks state under the lock and can
build a new candidate only while the version is positively absent. If a write
succeeded but its acknowledgement/inspection failed, do not assume it is absent.
Inspect the full-version tag and the recorded candidate digest first.

If the full-version tag exists but alias updates or post-write checks failed,
ordinary reruns intentionally refuse replacement. Obtain the exact preserved
index digest, review its source and attachments, then explicitly recover:

```sh
gh workflow run ci.yaml --repo Yaasha/near-e2ee-proxy --ref v1.2.3 \
  -f recover_digest=sha256:<preserved-index-digest>
```

Recovery reruns validation, checks the tag and main ancestry, verifies that the
full version still matches the supplied digest, and inspects all artifact
metadata and attachments. It skips rebuilding and never writes the full-version
tag. It can only advance eligible aliases; an intervening newer release remains
newer. Repeating recovery is safe. Prerelease recovery only verifies the preserved
artifact. A missing version or different digest is refused. If existing alias
metadata is inconsistent, investigate it rather than deleting preserved versions
or forcing an overwrite. Release preservation is deliberate: timestamps,
provenance and SBOMs can change on routine rebuilds, so rebuilding the same source
must not change the meaning of a previously published release. Publish a new patch
version for an intended rebuilt release.

Review the publication digest summary and both anonymous `public-pull` jobs for
every release or recovery. Pin the reviewed `@sha256:` digest for deployment using
the Compose procedure below; publication never restarts a running service. Registry
visibility and repository association remain administrative prerequisites.

### Release validation evidence

Maintained in-process command regressions use synthetic events, Git/source state,
authenticated HTTP registry responses and artifact fixtures. Environment, external
command execution and output belong to each command instance; ordinary policy
tests start no child processes or real Git repositories. This avoids repeated
race-detector process-exit delays while retaining the publication-policy cases.
The policy cases cover preservation, retries, recovery, numeric ordering, older
minor/patch releases, prerelease-to-stable
transitions and either serialized arrival order of competing publishers. Workflow
configuration regressions check the common publication lock, validation gates,
least-privilege token and immutable-source build. These tests do not execute
GitHub's scheduler or establish hosted registry permissions. Real Git/GitHub/Docker
integration is exercised by hosted publication. Run them through
`mise run distribution-test` or `mise run check`.

The release path has local synthetic policy/artifact coverage and retains the
existing two-architecture runtime validation. Local container regressions passed
on Docker Desktop for native ARM64 and emulated AMD64. The prior development
publication at `c21bba2` passed both native hosted container jobs, publication and
both anonymous pulls in
[run 40600221862](https://github.com/Yaasha/near-e2ee-proxy/actions/runs/40600221862).
The initial [v0.1.0 release](https://github.com/Yaasha/near-e2ee-proxy/releases/tag/v0.1.0)
passed hosted checks, both native container jobs, actual registry metadata and
attachment inspection, and both anonymous pulls in
[run 40607890984](https://github.com/Yaasha/near-e2ee-proxy/actions/runs/40607890984).
That run validates the released implementation; future command changes still
require their own validation. No application keys or paid inference are needed.

## First-publication visibility

A public source repository is insufficient evidence of an anonymously readable
package. Workflow-created packages can inherit repository access; test anonymous
pulls to establish the actual result. If the first package is private, its owner must open
[the package](https://github.com/users/Yaasha/packages/container/package/near-e2ee-proxy),
check its repository association, and use **Package settings → Danger Zone →
Change visibility → Public**, confirming the package name. GitHub documents that
making a package public cannot be reversed. This administrative action is not
performed by the publication workflow. See
[GitHub's package visibility instructions](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility).

Once main promotion succeeds, the separate `public-pull` jobs have read-only permissions and no login. They use
an empty Docker configuration, pull the published digest on both native Linux
architectures, and run `-help` with networking disabled, read-only filesystem and
reduced privileges. Private packages fail this check even when the publisher can
read them. A private or unlinked package blocks promotion; after correcting its
settings, rerun the failed publication job to inspect and promote an eligible
current-main build, followed by anonymous pulls. A missing hosted run,
failed publication, private package or failed anonymous pull remains unresolved;
local fixture tests alone do not establish public distribution.

For local anonymous inspection (substitute the digest from a successful summary):

```sh
image='ghcr.io/yaasha/near-e2ee-proxy@sha256:<index-digest>'
anonymous_config=$(mktemp -d)
DOCKER_CONFIG="$anonymous_config" docker pull --platform linux/amd64 "$image"
DOCKER_CONFIG="$anonymous_config" docker pull --platform linux/arm64 "$image"
rm -rf "$anonymous_config"
docker buildx imagetools inspect "$image"
docker buildx imagetools inspect "$image" --format '{{json .Provenance}}'
docker buildx imagetools inspect "$image" --format '{{json .SBOM}}'
```

These inspection commands follow [Docker's attestation format and inspection
documentation](https://docs.docker.com/build/metadata/attestations/).

## Deliberate Compose updates

Publication never deploys or restarts a service. Review the source change,
validation summaries, labels and attachments; record the chosen digest and retain
the previous digest for rollback. With the required runtime keys supplied through
your process environment, update explicitly:

```sh
export PROXY_IMAGE='ghcr.io/yaasha/near-e2ee-proxy@sha256:<reviewed-index-digest>'
docker compose --env-file /dev/null pull --policy always proxy
docker compose --env-file /dev/null up -d --no-build --pull never proxy
docker compose --env-file /dev/null ps
docker compose --env-file /dev/null exec proxy /near-e2ee-proxy -healthcheck
```

The image override retains all Compose hardening and environment-only settings.
`--no-build` is required for distribution use because the same Compose file also
supports local builds. To roll back, select the prior digest and repeat these
commands. The health probe checks local liveness only; perform authenticated
readiness deliberately when needed. Omit `PROXY_IMAGE` for the local build flow.

## Weekly infrastructure updates

Dependabot proposes GitHub Actions and Docker base updates every Monday at 09:00
Europe/London. It updates action commit pins, including the publication actions,
and Docker digests through reviewable PRs. Automatic merging is disabled in the
repository and no auto-merge workflow is configured. No `gomod` updater is enabled.
The `golang` rule ignores semantic-version updates while allowing digest refreshes
at the pinned compiler version; intentional Go-version changes must reconcile
`mise.toml`, `mise.lock`, `go.mod` and the build base together. The existing
compiler-drift gate rejects inconsistent base proposals before dependency download.
NEAR SDK and verification-policy changes remain manual.

Infrastructure update PRs use the same unprivileged lint, Go and both-platform
container gates as every other PR. Review upstream changes, action runtimes,
base-image contents/digests and licenses before merging; verify publication and
anonymous-pull summaries after the merge. Maintenance regressions run with the
repository's pinned Go through `mise run distribution-test` (also included in
`mise run check`); workflow and Dockerfile linting use the existing mise-locked
linters. Dependabot is GitHub-hosted; there is no unpinned local maintenance CLI
or application dependency change.
