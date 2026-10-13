# Compatibility

modelgate serves one public API, OpenAI Chat Completions
(`POST /v1/chat/completions`, plus `GET /v1/models`), in front of three
upstream APIs. The model table chooses the upstream per model:

| Upstream | Model table | What modelgate does |
|---|---|---|
| Anthropic Messages | `provider: anthropic` (the default) | Translates the request and the response, both ways, including streams. |
| OpenAI Chat Completions | `provider: openai` | Passes the request through, substituting the model name and a default output cap. |
| OpenAI Responses | `provider: openai`, `upstream_api: responses` | Translates the request and the response, both ways, including streams. |

This page is the per-field contract as of `v0.7.0`. It describes the code
in `internal/translate` and `internal/provider`; when the two disagree,
the code is right and this page is the defect.

## How to read the matrix

- **Supported** — sent upstream unchanged.
- **Translated** — sent as the upstream's equivalent field.
- **Rejected** — refused by modelgate with `400 invalid_request_error`
  before any upstream call; the message names the field, for example
  `response_format is not supported for anthropic models`.
- **Dropped** — accepted and not sent. Every drop is deliberate and listed
  here with its reason. A dropped sampling parameter is also named in the
  response header `X-Modelgate-Dropped-Params` (see below).

modelgate decodes every request into a fixed subset of the Chat
Completions request (`internal/oai`). A field outside that subset —
`metadata`, `store`, `service_tier`,
`modalities`, `audio`, `prediction`, `web_search_options`, `verbosity`
and any other — is **dropped on every upstream, the passthrough
included**. The matrix below covers the subset.

## Request fields

| Field | Anthropic Messages | OpenAI Chat Completions | OpenAI Responses |
|---|---|---|---|
| `model` | Translated: the table's `provider_model`. | Translated: the table's `provider_model`. | Translated: the table's `provider_model`. |
| `system` / `developer` messages | Translated: messages from any position in the history are joined, in order, with a blank line into one `system` text block, which carries the prefix cache breakpoint. | Supported. | Translated: joined into `instructions`. |
| `user` text content (string or `text` parts) | Translated: one text block per non-empty part; content with no text at all is rejected. | Supported. | Translated: `input_text` parts; content with no text is rejected. |
| `image_url`, `input_audio`, `file` parts | Rejected (`unsupported content part`). | Supported. | Rejected. |
| `assistant` text and `tool_calls` | Translated: a text block and `tool_use` blocks. | Supported. | Translated: a message item and `function_call` items correlated by `call_id` alone. |
| `tool_calls[].function.arguments` `""` | Translated: `{}` (a call that took no arguments). | Supported. | Translated: `{}`. |
| `tool_calls[].function.arguments` not JSON | Rejected. | Supported. | Rejected. |
| `assistant` with neither text nor tool calls (empty, `null` or whitespace-only content) | Skipped (since `v0.7.0`; rejected before): a stopped or regenerated reply leaves one in a chat client's history. Whitespace-only text beside tool calls is dropped and the calls kept. | Supported. | Skipped (since `v0.7.0`). |
| `tool` messages | Translated: consecutive tool messages become one user turn of `tool_result` blocks; text content only. | Supported. | Translated: `function_call_output` items; `tool_call_id` required. |
| message `name` | Dropped: the Messages API has no per-message name. | Supported. | Dropped: no per-item name. |
| `max_completion_tokens` | Translated: `max_tokens`. Preferred over `max_tokens` when both are set. | Supported. | Translated: `max_output_tokens`, which caps reasoning and visible output together. Preferred over `max_tokens`. |
| `max_tokens` | Translated: `max_tokens`. | Supported. | Translated: `max_output_tokens`. |
| neither cap | The row's `default_max_tokens`, or the gateway's `DEFAULT_MAX_TOKENS` when the row sets none. | `max_completion_tokens` set to the same default. | `max_output_tokens` set to the same default. |
| `temperature`, `top_p` | Translated when the row allows sampling and no `reasoning_effort` is set; dropped when the row declares `sampling: false` or an effort is set, because effort-capable models refuse them. | Supported; dropped when the row declares `sampling: false`. | Dropped unless the row declares `sampling: true`: the reasoning models behind this upstream accept only their defaults. |
| `frequency_penalty`, `presence_penalty`, `seed`, `logit_bias` | Dropped (since `v0.7.0`; rejected before): the Messages API has no equivalent. | Supported (`logit_bias` since `v0.7.0`). | Dropped (since `v0.7.0`; rejected before). |
| `n` | `1` accepted; anything else rejected. | Supported. | `1` accepted; anything else rejected. |
| `logprobs`, `top_logprobs` | Dropped (since `v0.7.0`): the response carries no log probabilities. | Supported, and the returned `logprobs` reach the client (since `v0.6.0`). | Dropped (since `v0.7.0`). |
| `stop` | Translated: `stop_sequences`; empty strings are ignored. | Supported. | Dropped (since `v0.7.0`): the Responses API has no stop parameter. A value that is neither a string nor a list of strings is rejected. |
| `stream` | Translated. | Supported. | Translated. |
| `stream_options.include_usage` | The gateway emits a final usage chunk when asked. | The gateway always asks the upstream for usage (so an aborted stream can be billed) and forwards the usage chunk only when the client asked. | The gateway emits a final usage chunk when asked. |
| `tools` (type `function`) | Translated: `name`, `description`, `input_schema`; a tool with no `parameters` gets `{"type":"object"}`. | Supported. | Translated: flat function tools with `strict: false`. |
| `tools` of any other type | Rejected. | Supported. | Rejected. |
| `tool_choice` | Translated: `auto`→`auto`, `none`→`none`, `required`→`any`, a named function→`tool`. Anything else rejected. | Supported. | Translated: `auto`, `none`, `required`, a named function. Anything else rejected. |
| `parallel_tool_calls` | `false` is translated to `tool_choice.disable_parallel_tool_use: true` (with `tool_choice: auto` when none was given). Ignored under `tool_choice: none` and without tools, where it has nothing to limit. `true` is the upstream default. | Supported. | Supported. |
| `reasoning_effort` | Translated: `output_config.effort`; `none` and `minimal` become `low`, and `low` through `max` map to themselves. An unknown level is rejected. | Supported (not validated). | Translated: `reasoning.effort`; an unknown level is rejected. |
| `reasoning_effort` outside a model's declared `reasoning_efforts` | Rejected. | Rejected. | Rejected. |
| forced `tool_choice` on a model declared `forced_tool_choice: false` | Rejected. | Rejected. | Rejected. |
| `response_format` | Rejected. | Supported. | Translated: `text.format` (`text`, `json_object`, and `json_schema` with a name and a schema). |
| `prompt_cache_key` | Dropped (not named in the header): caching is positional on this upstream (see Prompt caching). | Forwarded (since `v0.7.0`) as a 32-character digest of the gateway key's id and the client's value, so two keys never share a routing bucket. | Translated the same way (since `v0.7.0`). |
| `user` | Translated: the hex SHA-256 of the value becomes `metadata.user_id` (since `v0.6.0`). Anthropic wants an opaque id of at most 512 characters with no name, email address or phone number in it; the digest is always 64 characters and stays stable per end user. | Supported. | Translated: `user` (since `v0.6.0`). |

Claude models released after Claude Opus 4.6 accept only `temperature: 1.0`
and `top_p >= 0.99`, and answer any other value on a request that sets no
`reasoning_effort` with a 400 (the upstream's reason is in the log). A row
for such a model should declare `sampling: false`, so modelgate drops both
parameters, names them in `X-Modelgate-Dropped-Params`, and the chat
client's default `temperature` no longer fails the request.

Some models refuse part of what modelgate translates: Claude Fable 5.1 and
Claude Opus 5.5 refuse a forced tool choice (`required` or a named
function), GPT-6 Sol and Luna refuse `minimal` effort, and Claude Haiku
4.5 refuses `output_config.effort`. The model table can declare the first
two (`forced_tool_choice: false`, `reasoning_efforts: [...]`), and then
modelgate refuses such a request itself, before any upstream call, with a
`400 invalid_request_error` whose message names the model and the rule —
on every upstream, the passthrough included. Anything undeclared reaches
the upstream, which answers 400; the client gets `400
invalid_request_error` with modelgate's fixed message, and the upstream's
reason is in the log.

### `X-Modelgate-Dropped-Params`

A chat client sends sampling parameters by default, so the translated
upstreams drop the ones they cannot honour instead of refusing the
request. The response — a JSON body or a stream — names what was removed:

```
X-Modelgate-Dropped-Params: temperature, seed
```

The value lists, comma-and-space separated and always in this order, the
parameters the request carried that were not forwarded: `temperature`,
`top_p`, `frequency_penalty`, `presence_penalty`, `seed`, `logprobs`,
`top_logprobs`, `logit_bias`, `stop`. A neutral value is not a drop
(`frequency_penalty: 0`, `logprobs: false`, `top_logprobs: 0`,
`logit_bias: {}`, an empty `stop`, any `null`), and the header is absent
when nothing was dropped.

The header names every parameter actually removed from the upstream
request, so what it reports and what the upstream received never differ.
That includes the case a row cannot override: on Anthropic rows a request's
`reasoning_effort` displaces `temperature` and `top_p` even when the row
declares `sampling: true`, because the models that accept an effort refuse
both, and the header then names them. A parameter outside the decoded
subset is never listed, and neither is `prompt_cache_key`.

## Response fields

**Anthropic.** Text blocks become `message.content`; `tool_use` blocks
become `tool_calls` with the input as the arguments string (`{}` when the
block had none). Thinking blocks are neither returned nor counted as
`reasoning_tokens`. `stop_reason` becomes `finish_reason`:

| `stop_reason` | `finish_reason` |
|---|---|
| `end_turn`, `stop_sequence`, `pause_turn`, anything unknown | `stop` |
| `max_tokens`, `model_context_window_exceeded` | `length` |
| `tool_use` | `tool_calls` |
| `refusal` | `content_filter` |

A stream carries the same mapping. A `tool_use` block that closes without
streaming any argument text — Anthropic's shape for a call to a tool that
takes no arguments — sends `{}` as its arguments when the block stops, so
a client accumulating the stream holds parseable arguments and can echo
them back.

Usage: `prompt_tokens` is `input_tokens + cache_creation_input_tokens +
cache_read_input_tokens`, `prompt_tokens_details.cached_tokens` is
`cache_read_input_tokens`, and the cache-write and cache-read counts are
booked separately at the model table's cache prices.

**OpenAI Chat Completions.** Passed through, with the public model name
restored.

**OpenAI Responses.** Output text, refusals and function calls are
translated back; reasoning items are dropped, and their token count is
reported as `completion_tokens_details.reasoning_tokens`.

## Prompt caching

On Anthropic models modelgate turns prompt caching on by default. Every
request carries two of the four allowed breakpoints:

1. An explicit `cache_control: {"type": "ephemeral"}` on the `system`
   block, which caches the tools and the system prompt together (tools
   render before system). With no system prompt it goes on the last tool
   instead; with neither there is no explicit breakpoint.
2. The request-level `cache_control: {"type": "ephemeral"}`, which the API
   places on the last cacheable block and moves forward as the
   conversation grows, so each turn of an agent loop reads the previous
   turn's prefix.

Both carry the row's lifetime. With no `cache_ttl`, or `"5m"`, neither
carries a `ttl` and both use the five-minute cache: a write costs 1.25×
the input price and a read about 0.1×. With `cache_ttl: "1h"` both carry
`{"type": "ephemeral", "ttl": "1h"}`: a write costs 2× the input price,
reads are unchanged, and a conversation resumed within the hour reads its
prefix back instead of rewriting it. People pause longer than five
minutes between chat turns, so the one-hour lifetime is the one for
models serving a chat interface; set the row's `cache_write_usd_per_mtok`
to the price of the lifetime the row asks for. Every breakpoint in a
request carries the same lifetime, because the API refuses a longer one
placed after a shorter one.

A prefix shorter than the model's minimum cacheable length (512 tokens on
the current Claude models, up to 4096 on older ones) is not cached and
costs nothing extra, so one-off short prompts pay no write premium.

System and developer messages are lifted into that cached system block
from wherever they sit in the history. A client that adds or rewrites a
system or developer message partway through a conversation therefore
changes the front of the prefix, and every turn writes the whole
conversation to the cache at the write price and reads nothing back,
which costs more than sending it uncached.

`translate.AnthropicOptions.DisablePromptCaching` turns caching off for a
request; no model-table field sets it yet, so every Anthropic model
caches.

On OpenAI models caching is automatic upstream; modelgate books the
reported `cached_tokens` as cache reads. A client's `prompt_cache_key` is
forwarded as a digest namespaced by the gateway key. The provider's
extended-retention option is not sent: it is owed until its name, values
and price are confirmed.

## Errors

The upstream's error body never reaches the client: modelgate answers
with its own code and a fixed message, so a provider cannot dictate what
the gateway says.

| Upstream answer | Provider error | Client sees |
|---|---|---|
| 400 whose message says the prompt exceeds the context window, or an OpenAI error with code `context_length_exceeded` | `provider.ErrContextLengthExceeded` (also an `ErrInvalidRequest`) | `400 invalid_request_error` with code `context_length_exceeded` and `param: messages` |
| any other 4xx except 401, 403, 408 and 429 | `ErrInvalidRequest` | `400 invalid_request_error` |
| 401, 403 | `ErrAuth` | `502 provider_auth_error` |
| 408 | `ErrTimeout` | `504 timeout` |
| 429 | `ErrRateLimited` (retried) | `429 rate_limited` |
| 5xx, 529, transport failure | `ErrUnavailable` (retried) | `503 provider_unavailable` |

Retries: up to three attempts with exponential backoff, for 429, 5xx and
transport failures; a stream is retried only while no event has reached
the client.

Every refusal is logged at WARN — Anthropic's as `anthropic upstream
rejected the request` with the status, the error type and the request id;
OpenAI's with the status, path and error code — and the upstream's message
alongside, truncated to 512 bytes with anything credential-shaped
(`sk-…`, `Bearer …`) replaced by `[redacted]`. The message is left out of
401 and 403 records, whose bodies can quote the key. An `error` event
inside an Anthropic stream is logged the same way. modelgate never logs a
request body, so prompt text reaches the log only if an upstream quotes it
in an error message.

## Streams

A streamed call is aborted when the upstream sends nothing — response
headers included — for `provider.DefaultStreamIdleTimeout` (five
minutes), inside the request deadline. The abort is an `ErrTimeout`: the
client gets `504 timeout` if nothing has streamed yet, or an in-band error
chunk if it has. The window is generous on purpose: a healthy stream can
go quiet while the model reasons before its first output token. Each
client takes a different window through `SetStreamIdleTimeout` (zero turns
the abort off); non-streamed calls are bounded by the request deadline
alone.

## Not supported

Documented, not built:

- Images, audio and files on the translated upstreams.
- `n > 1` on the translated upstreams.
- `response_format` on Anthropic models (the Messages API's structured
  outputs are a different shape and are not translated).
- Returning Claude's thinking, or Responses reasoning items, to the client.
  A Chat Completions history cannot carry them back either, so a
  multi-turn Claude conversation continues without its earlier thinking
  blocks. The Anthropic documentation asks for thinking blocks to be
  passed back unchanged on the same model; a Claude tool-result round trip
  through modelgate has not yet been probed live, so what the upstream
  does with the omission on current models is unrecorded. Before Claude
  agent loops rely on `v0.6.0`, a short-lived-key probe owes the answer: a
  streamed `claude-opus-5-5` tool call through the gateway, then the same
  history echoed back with the tool result. A 400 there means the request
  path needs a way to keep thinking blocks, which is a separate design
  decision.
- Anthropic server tools (web search, code execution), citations, PDFs,
  batches, token counting, and the public Responses API endpoint.
