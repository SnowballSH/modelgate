# Changelog

## v0.5.0 — 2026-09-07

- `reasoning_effort` on Anthropic models is translated to `output_config.effort`
  (`none`/`minimal` → `low`; unknown levels are a 400) instead of rejected.
- Behaviour change on Anthropic models: `temperature` and `top_p` are dropped
  whenever `reasoning_effort` is set. v0.4.0 rejected such a request itself, so
  it never reached the upstream; v0.5.0 translates the effort and sends the
  request without its sampling parameters, because models released after Claude
  Opus 4.6 reject a temperature other than 1.0 and a `top_p` below 0.99.
- `developer` messages fold into the Anthropic `system` prompt.
- A `stop` of `null`, of `""`, or of a list with no non-empty entry is read as
  unset on both upstreams: Anthropic no longer receives `stop_sequences: [""]`,
  and the Responses upstream accepts such a request instead of a 400.
- Model table: `upstream_api: responses` (OpenAI models only) routes a model
  through the Responses API; Chat Completions stays the default. OpenAI
  documents GPT-5.6 function tools on Chat Completions as compatible only
  with reasoning `none`.
- `usage.completion_tokens_details.reasoning_tokens` is reported when the
  upstream provides it; reasoning tokens are booked as output tokens.
- Known limitation of the Responses upstream: Chat Completions carries no
  reasoning items, so modelgate never echoes them and the model re-reasons
  after every tool result; `max_tokens` caps reasoning and output together.
- Every public request now logs one INFO record, `msg=request`, with
  `key_id`, `model`, `upstream`, `reasoning_effort`, `stream` and `status`.
  Fields carry only values resolved against the model table and the accepted
  effort vocabulary, never a raw request string.
- An OpenAI upstream 4xx or 5xx now logs its `error.message` at WARN as
  `openai upstream rejected the request`, with `path`, `status` and `message`,
  redacted of credential-shaped text and truncated. A 401 or 403 logs no
  message: those bodies quote part of the refused key. The message never
  reaches the client's error body.
- `--version` prints the release version.
