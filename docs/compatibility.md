# Compatibility

modelgate serves one public API, OpenAI Chat Completions
(`POST /v1/chat/completions`, plus `GET /v1/models`), in front of three
upstream APIs. The model table chooses the upstream per model:

| Upstream | Model table | What modelgate does |
|---|---|---|
| Anthropic Messages | `provider: anthropic` (the default) | Translates the request and the response, both ways, including streams. |
| OpenAI Chat Completions | `provider: openai` | Passes the request through, substituting the model name and a default output cap. |
| OpenAI Responses | `provider: openai`, `upstream_api: responses` | Translates the request and the response, both ways, including streams. |

This page is the per-field contract as of `v0.6.0`. It describes the code
in `internal/translate` and `internal/provider`; when the two disagree,
the code is right and this page is the defect.

## How to read the matrix

- **Supported** — sent upstream unchanged.
- **Translated** — sent as the upstream's equivalent field.
- **Rejected** — refused by modelgate with `400 invalid_request_error`
  before any upstream call; the message names the field, for example
  `seed is not supported for anthropic models`.
- **Dropped** — accepted and not sent. Every drop is deliberate and listed
  here with its reason.

modelgate decodes every request into a fixed subset of the Chat
Completions request (`internal/oai`). A field outside that subset —
`prompt_cache_key`, `metadata`, `store`, `service_tier`, `logit_bias`,
`modalities`, `audio`, `prediction`, `web_search_options`, `verbosity`
and any other — is **dropped on every upstream, the passthrough
included**. The matrix below covers the subset.

## Request fields

| Field | Anthropic Messages | OpenAI Chat Completions | OpenAI Responses |
|---|---|---|---|
| `model` | Translated: the table's `provider_model`. | Translated: the table's `provider_model`. | Translated: the table's `provider_model`. |
| `system` / `developer` messages | Translated: joined with a blank line into one `system` text block, which carries the prefix cache breakpoint. | Supported. | Translated: joined into `instructions`. |
| `user` text content (string or `text` parts) | Translated: one text block per non-empty part; content with no text at all is rejected. | Supported. | Translated: `input_text` parts; content with no text is rejected. |
| `image_url`, `input_audio`, `file` parts | Rejected (`unsupported content part`). | Supported. | Rejected. |
| `assistant` text and `tool_calls` | Translated: a text block and `tool_use` blocks. | Supported. | Translated: a message item and `function_call` items correlated by `call_id` alone. |
| `tool_calls[].function.arguments` `""` | Translated: `{}` (a call that took no arguments). | Supported. | Translated: `{}`. |
| `tool_calls[].function.arguments` not JSON | Rejected. | Supported. | Rejected. |
| `assistant` with neither text nor tool calls | Rejected. | Supported. | Rejected. |
| `tool` messages | Translated: consecutive tool messages become one user turn of `tool_result` blocks; text content only. | Supported. | Translated: `function_call_output` items; `tool_call_id` required. |
| message `name` | Dropped: the Messages API has no per-message name. | Supported. | Dropped: no per-item name. |
| `max_completion_tokens` | Translated: `max_tokens`. Preferred over `max_tokens` when both are set. | Supported. | Translated: `max_output_tokens`, which caps reasoning and visible output together. Preferred over `max_tokens`. |
| `max_tokens` | Translated: `max_tokens`. | Supported. | Translated: `max_output_tokens`. |
| neither cap | The gateway's default (`DEFAULT_MAX_TOKENS`). | `max_completion_tokens` set to the gateway's default. | `max_output_tokens` set to the gateway's default. |
| `temperature`, `top_p` | Translated when no `reasoning_effort` is set; dropped when one is, because effort-capable models refuse them. Models released after Claude Opus 4.6 accept only `temperature: 1.0` and `top_p >= 0.99`; other values come back as a 400 with the upstream's reason in the log. | Supported. | Dropped: the reasoning models behind this upstream accept only their defaults. |
| `frequency_penalty`, `presence_penalty`, `seed` | Rejected. | Supported. | Rejected. |
| `n` | `1` accepted; anything else rejected. | Supported. | `1` accepted; anything else rejected. |
| `logprobs`, `top_logprobs` | `logprobs: true` and `top_logprobs > 0` rejected; `false` and `0` accepted. | Supported, and the returned `logprobs` reach the client (since `v0.6.0`). | `logprobs: true` and `top_logprobs > 0` rejected. |
| `stop` | Translated: `stop_sequences`; empty strings are ignored. | Supported. | Rejected when it names any sequence: the Responses API has no stop parameter. `null`, `""` and `[]` are accepted. |
| `stream` | Translated. | Supported. | Translated. |
| `stream_options.include_usage` | The gateway emits a final usage chunk when asked. | The gateway always asks the upstream for usage (so an aborted stream can be billed) and forwards the usage chunk only when the client asked. | The gateway emits a final usage chunk when asked. |
| `tools` (type `function`) | Translated: `name`, `description`, `input_schema`; a tool with no `parameters` gets `{"type":"object"}`. | Supported. | Translated: flat function tools with `strict: false`. |
| `tools` of any other type | Rejected. | Supported. | Rejected. |
| `tool_choice` | Translated: `auto`→`auto`, `none`→`none`, `required`→`any`, a named function→`tool`. Anything else rejected. | Supported. | Translated: `auto`, `none`, `required`, a named function. Anything else rejected. |
| `parallel_tool_calls` | `false` is translated to `tool_choice.disable_parallel_tool_use: true` (with `tool_choice: auto` when none was given). Ignored under `tool_choice: none` and without tools, where it has nothing to limit. `true` is the upstream default. | Supported. | Supported. |
| `reasoning_effort` | Translated: `output_config.effort`; `none` and `minimal` become `low`, and `low` through `max` map to themselves. An unknown level is rejected. | Supported (not validated). | Translated: `reasoning.effort`; an unknown level is rejected. |
| `response_format` | Rejected. | Supported. | Translated: `text.format` (`text`, `json_object`, and `json_schema` with a name and a schema). |
| `user` | Translated: `metadata.user_id` (since `v0.6.0`). | Supported. | Translated: `user` (since `v0.6.0`). |

Some Claude models refuse part of what modelgate translates: Claude Fable
5.1 and Claude Opus 5.5 refuse a forced tool choice (`required` or a named
function), and Claude Haiku 4.5 refuses `output_config.effort`. modelgate
does not keep a per-model capability list; the upstream answers 400, the
client gets `400 invalid_request_error`, and the upstream's reason is in
the log.

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

Both use the five-minute cache. A prefix shorter than the model's minimum
cacheable length (512 tokens on Claude Opus 5, 1024 on Claude Sonnet 5 and
Opus 4.8, up to 4096 on older models) is not cached and costs nothing
extra. A cached write costs 1.25× the input price and a read about 0.1×;
two requests sharing a prefix within five minutes already break even. A
workload of one-off prompts pays the write premium with nothing to read
back.

The switch is `translate.AnthropicOptions.DisablePromptCaching`, set per
model through `translate.ToAnthropicWith`. `translate.ToAnthropic` — the
entry point the server calls in `v0.6.0` — always caches; carrying a
per-model flag from the model table to that option is the server's side of
the seam.

On OpenAI models caching is automatic upstream; modelgate books the
reported `cached_tokens` as cache reads.

## Errors

The upstream's error body never reaches the client: modelgate answers
with its own code and a fixed message, so a provider cannot dictate what
the gateway says.

| Upstream answer | Provider error | Client sees |
|---|---|---|
| 400 whose message says the prompt exceeds the context window, or an OpenAI error with code `context_length_exceeded` | `provider.ErrContextLengthExceeded` (also an `ErrInvalidRequest`) | `400 invalid_request_error` today; the server can single it out as `context_length_exceeded` by testing for the new error first. |
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
- `n > 1`, log probabilities, `frequency_penalty`, `presence_penalty` and
  `seed` on the translated upstreams.
- `stop` on the Responses upstream.
- `response_format` on Anthropic models (the Messages API's structured
  outputs are a different shape and are not translated).
- Returning Claude's thinking, or Responses reasoning items, to the client.
  A Chat Completions history cannot carry them back either, so a
  multi-turn Claude conversation continues without its earlier thinking
  blocks. The Anthropic documentation asks for thinking blocks to be
  passed back unchanged on the same model; a Claude tool-result round trip
  through modelgate has not yet been probed live, so what the upstream
  does with the omission on current models is unrecorded.
- Anthropic server tools (web search, code execution), citations, PDFs,
  batches, token counting, and the public Responses API endpoint.
