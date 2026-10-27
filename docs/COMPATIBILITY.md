# Chat Completions contract

This is a deliberately limited OpenAI-compatible service. Unsupported inputs fail
locally with an OpenAI-shaped error instead of being forwarded or silently dropped.

## Routes

| Route | Behavior |
| --- | --- |
| `POST /v1/chat/completions` | Verified JSON or provisional text SSE; native function tools |
| `GET /v1/models` | Configured discovery entries; each must pass hardware admission; not an exhaustive catalog |
| `GET /readyz` | Checks configured discovery entries without billable inference |
| `GET /healthz` | Process liveness only; no authentication or upstream request |
| Other routes/methods | Unsupported; no wildcard forwarding |

Every route except liveness requires one `Authorization: Bearer <proxy token>`
header. No website is served. Duplicate authentication headers are rejected. Query
parameters and caller-supplied upstream encryption/routing headers are unsupported.
None of the caller's headers are forwarded to NEAR; the SDK constructs its own
request.

## Accepted input

- A canonical `model` selected per request, with at least one and at most 1,024 messages.
- Roles: `system`, `developer`, `user`, `assistant`, `tool`.
- String content or text-only parts (`type: text`, `text`). Assistant content may
  be omitted/null for function calls. Tool content contains the tool result.
- Assistant `reasoning_content`, `reasoning`, `refusal`, and message `name` where
  applicable; the SDK encrypts these fields.
- Native function `tools`: name, description, parameter schema, optional strict
  flag. Schemas are encrypted as a whole and are not interpreted by this proxy.
- `tool_choice`: `none`, `auto`, `required`, or a named declared function.
- `parallel_tool_calls`, assistant `tool_calls`, tool-result `tool_call_id`.
- `temperature` (0–2), `top_p` (0–1), presence/frequency penalties (-2–2), `seed`.
- `max_tokens` or `max_completion_tokens`, from 1 to 8,192; not both. Default:
  1,024 completion tokens. Only one choice (`n: 1`) is supported.
- `reasoning_effort`: `none`, `minimal`, `low`, `medium`, `high`.
- `chat_template_kwargs`: only boolean `enable_thinking`.
- `response_format`: only `text` or `json_object`.
- `stream` and `stream_options.include_usage`.

Availability and behavior of sampling, reasoning and formatting options still
depend on the selected model. The proxy validates types and confidentiality;
it does not promise a model supports every accepted option.

The model does not have to appear in `MODELS`. That setting controls discovery
and readiness only. Model IDs are 1–200 ASCII letters/digits, underscores,
hyphens, dots or slashes, starting with a letter/digit/underscore/hyphen; `..` is
rejected. The SDK disables aliasing and verifies the requested model before
inference. Missing/invalid confidential evidence fails with a safe 502 error;
catalog availability alone never authorizes inference.

Function names use 1–64 letters/digits/underscores/hyphens. Call IDs use 1–256 of
the same characters and must be unique in the replayed history. Tool results must
match preceding calls, and all pending calls must have results before generation
continues. Function arguments must be valid JSON objects. Tool execution happens
on the client, not in the proxy or through NEAR's hosted tools.

Inputs outside this list are unsupported, including arbitrary metadata, `user`,
`stop`, legacy `functions`/`function_call`, logprobs, multimodal content, structured
output schemas, hosted web search, Fusion, storage and provider extensions.
Unknown fields, noncanonical key casing, duplicate keys (also inside schemas),
trailing JSON values, invalid UTF-8 and nesting beyond 64 levels are rejected.

## Output and streaming

JSON requests use nonstreaming upstream inference. No JSON answer leaves until
its `provider_tee` receipt and output contract verify.

SSE requests use the SDK's native encrypted upstream stream. Validated role,
text/reasoning/refusal deltas flush immediately and remain provisional. The SDK
retains exact encrypted wire bytes through EOF; only a valid `provider_tee` receipt
permits tool calls, finish reasons, requested usage and `[DONE]`. Function fragments
are assembled and validated before releasing one complete indexed delta per call.
Tool-only responses may therefore wait until inference and verification finish.
There are no keepalive comments. Allow for cold attestation and client/proxy timeouts;
disable response buffering in your reverse proxy if you want incremental display.

SSE responses carry `X-Response-Verification: end-of-stream`, an informational
header, not a proof clients verify themselves. After text starts, failures use
`event: error` with a fixed OpenAI-shaped error and close without a finish event or
`[DONE]`; the HTTP status is already 200. Clients must handle this as failure.
Before any SSE output, failures use ordinary JSON HTTP errors. Standard clients
may already act on provisional text; see [the tradeoff](SECURITY.md#streaming-tradeoff).

Output supports `stop`, `length`, `tool_calls` and `content_filter` finish reasons.
Malformed/incomplete tool arguments are rejected rather than released for execution.
Numeric/null SGLang `matched_stop` metadata is discarded; free-text values fail.
SGLang's numeric `usage.reasoning_tokens` is mapped to completion-token details.
NEAR may refresh `created` between chunks; local chunks keep the first timestamp.
Response ID and model must remain the same. Other unknown output fields are rejected.

## Limits and error categories

| Limit | Value |
| --- | --- |
| Active HTTP operations | Four; excess requests receive 429 |
| Request body | 1 MiB |
| JSON nesting | 64 levels |
| Tools | 128 |
| Upstream encrypted request/response | SDK limit: 32 MiB each |
| Decrypted response returned locally | 8 MiB |
| Decrypted SSE line/event | 1 MiB |
| SDK retained receipt bodies | 128 MiB combined budget; 16 cache entries |
| Attestation cache | Five minutes |
| Receipt retention | One minute after capture |
| Inference/verification deadline | Three minutes |

The limits bound individual buffers and caches, not total process RAM. Verification
libraries and concurrent callers need additional memory. The SDK's shared
verification work can briefly outlive a cancelled caller, with its own bounded
deadline; closing the client cancels it. Raw upstream errors are
never relayed. Auth errors use 401, unsupported inputs 400/415, size errors 413,
overload 429, unavailable readiness 503, upstream/verification/shape errors 502,
and elapsed/cancelled request contexts 504 where the connection is still writable.

## What was actually tested

Live `Qwen/Qwen3.8-27B` checks cover JSON text, incremental SSE with usage, a streamed
native function call, and assistant/tool history continuation. Every successful answer
passed real CPU/GPU verification, E2EE and model-receipt verification. The suite
uses a real local HTTP server and a real NEAR backend.

Offline tests cover authentication, unsupported fields and routes, size limits,
overload, cancellation, strict evidence policy, receipt failure before tool release,
provisional text flushing, fragmented tool assembly, SSE truncation/limits and
dependency encryption/signature contracts.

No particular LiteLLM, Hermes, OpenCode, Orion, OpenAI SDK or external bridge version
has been integration-tested. Configure a custom Chat Completions provider using
the base URL/key/model from README.md and report rejected required fields before
expanding the contract. Responses support is deferred; it is not forwarded to NEAR.
