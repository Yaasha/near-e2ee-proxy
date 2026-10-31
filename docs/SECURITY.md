# Security boundary

The proxy provides verified hardware admission and encrypted inference under the
following trust assumptions. Review these before sending sensitive workloads.

## What is trusted

- Your client, proxy host, its administrators and any local TLS terminator see
  plaintext. A container does not protect data from its own host administrator.
- NEAR's deployment/update controls, key-release controls and inference software
  are trusted to handle plaintext correctly inside the confidential environment.
- The SDK, hardware vendors, firmware and verification trust roots are trusted.
  In particular, NVIDIA verification-key discovery currently relies on ordinary
  HTTPS. The proxy does not promise independence from certificate-authority failure.

Hardware attestation is not an independent audit of the full effective workload.
The project does not establish the current complete engine/sidecar inventory,
an enforced remote outbound-network policy, immutable model-weight enforcement,
or absence of administrator-authorized software changes. Model signing keys may
be shared across instances/configurations, so receipts do not identify one VM.

## What the proxy enforces

1. A separate shared proxy bearer token, compared through fixed-size hashes in
   constant time, before body parsing or upstream work. The NEAR key stays local.
2. Bounded, recursively validated requests. Every accepted content-bearing field
   has known SDK encryption coverage. Unknown fields do not pass through.
3. The SDK's real Intel TDX verifier, debug rejection, `UpToDate` CPU policy with
   no advisory IDs, fresh nonce/key binding, measured configuration/event checks,
   mandatory NVIDIA GPU evidence for every candidate, and gateway TLS binding.
4. Ed25519/v2 model-key E2EE with expanded field encryption. Canonical model IDs,
   disabled aliasing and model-key routing. There is no insecure fallback mode.
5. Exactly one upstream inference attempt, followed by model-receipt verification
   against the SDK's retained exact encrypted bytes and preflight evidence.
   Gateway-only signatures cannot satisfy this requirement. Only transient receipt
   retrieval may retry, with a bounded delay and the request deadline.
6. Output validation before delivery. JSON answers and SSE tool calls/terminal
   events wait for verification; streaming text is provisional as explained below.
   New local JSON/SSE encoding is not itself covered by the upstream signature.

Successful preflight evidence may be reused for five minutes; this is not a new
hardware challenge for each token or request. All returned candidates must verify,
but the provider's returned set is not a complete fleet inventory.

Clients choose the model in each request. `MODELS` supplies discovery/readiness
entries and does not restrict that choice. The selected model's evidence is
verified before its key is used; an advertised model or an arbitrary model name
cannot bypass CPU/GPU admission or the E2EE/receipt requirements.

## Streaming tradeoff

Streaming prioritizes early text display. Pre-request attestation, model-key
binding and expanded input E2EE remain mandatory. Prompts, tool definitions and
tool results are encrypted before they leave the trusted proxy. Delaying the
response signature check does not send those fields in plaintext or give
intermediaries the model's decryption key.

Text, reasoning and refusals are provisional until the final model receipt passes.
AEAD decryption alone does not prove model origin: someone who knows the recipient
public key can encrypt fabricated replies. The final signature authenticates the
model's exact wire exchange. An invalid signature yields an explicit stream error,
with no tool calls, finish reason, usage or `[DONE]`. Already displayed/consumed
text cannot be retracted. Nonstreaming JSON retains full pre-release verification.

Withholding native tool calls limits one execution path. It does not prevent a
client from treating provisional text as instructions, executing code extracted
from it, or sending sensitive data in a later request because of a forged answer.
Use nonstreaming JSON where such output authenticity is required before any use.
This change preserves request encryption under the existing trust assumptions;
it is not a guarantee that all downstream workflows preserve confidentiality.

## Data and operational behavior

The application has no database, conversation store, telemetry or request-body
logging. Errors use fixed messages and do not expose upstream text. Logs contain
only fixed startup/shutdown messages. Plaintext and keys still occupy normal Go
memory; reliable memory zeroization is not promised. The SDK retains bounded
encrypted wire bodies briefly for signature verification.

Encrypted content includes messages, reasoning, function descriptions/schemas,
function names/arguments, tool results and answers. Model selection, roles, opaque
tool-call IDs, sampling/token settings, timing, lengths and account/network
metadata remain observable. Do not encode private content into protocol IDs.
Field encryption does not provide full forward secrecy against later compromise
of a long-lived recipient model key.

The top-level `enable_thinking` alias accepts only a boolean and normalizes to
`chat_template_kwargs.enable_thinking` before the request reaches the SDK. This
control remains observable protocol metadata; prompts and tool data retain the
existing encryption requirements. Conflicting controls fail locally. The
`thinking_budget` extension accepts only integers from 0 to 32,768 and remains
observable numeric metadata through the SDK. It cannot carry arbitrary text or
objects. The proxy forwards the budget unchanged; backend support and enforcement
are model-dependent. It never changes completion-token limits or request
deadlines in response to a budget. Invalid controls receive fixed public field
names and explanations; errors never include values or arbitrary field names.

Local HTTP is appropriate for loopback. Use HTTPS or an encrypted tunnel when
traffic crosses other machines. Avoid HTTP debug dumps, request-body capture and
client/provider fallbacks outside the intended trust boundary.

The app fixes NEAR's upstream origin, disables ambient HTTP proxies on that
transport, rejects redirects and never forwards caller headers. CPU/GPU verifiers
also contact their verification services. Image provenance is not independently
verified on every request.

## Limits of the claim

The container example runs as UID/GID 65532, uses a read-only root filesystem,
drops Linux capabilities and enables `no-new-privileges`. Host publishing is bound
to loopback. Its liveness probe calls only local `/healthz`, with a two-second
deadline and no credentials, ambient HTTP proxy or redirects. Authenticated
`/readyz` remains a separate hardware-admission operation and is not a periodic
container health check. See [container operations](CONTAINERS.md) for trusted TLS
termination/tunnel guidance and the validation limits. Container hardening does
not change the trusted-host, deployment-control, E2EE or receipt assumptions above.

Development and guarded versioned image publication add OCI source/revision
labels, build provenance and an SBOM, inspected from GHCR before alias promotion. Build provenance
describes the image build; it does not independently approve NEAR's live software,
deployment/update controls or outbound-network policy. Anonymous pull jobs prove
registry distribution and isolated CLI startup only. Full-version preservation
and monotonic aliases rely on trusted package administrators and every publisher
sharing the workflow lock. They are not registry-enforced immutability. Operators
review a digest and update deliberately; publication does not deploy a running
service. See
[image distribution](CONTAINERS.md#development-images-on-ghcr).

The project has not undergone an independent security audit. It does not
promise protection against invasive physical attacks, undisclosed hardware bugs,
malicious trusted software, compromised clients/proxy hosts, or all metadata
leakage. Do not describe the existing checks as an unconditional guarantee that
NEAR could never obtain plaintext under every possible failure scenario.
