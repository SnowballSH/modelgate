# Changelog

## v0.7.0 — unreleased

A v0.6.0 model table loads unchanged, and a row carrying none of the new
fields is served as before. What changes for such a row is listed under
Changed.

### Changed

- `frequency_penalty`, `presence_penalty`, `seed`, `logprobs`,
  `top_logprobs` and `logit_bias` are dropped on Anthropic and Responses
  models instead of answering 400, and so is `stop` on Responses models.
  A chat client sends these by default. The response names what was
  removed in `X-Modelgate-Dropped-Params`, on JSON and streamed responses
  and on an upstream error that follows a successful translation, and
  it also names `temperature` and `top_p` when a row or a
  `reasoning_effort` displaces them.
- An assistant message with no text and no tool calls is skipped instead
  of answering 400, and whitespace-only assistant text is not sent.
- A request is translated before it takes a concurrency slot or reserves
  cost, and the output bounds are checked after the rate limit and the
  model resolve. Client-visible: a request the gateway cannot translate
  answers 400 even while every slot is taken or a cap is spent, where
  v0.6.0 answered 429; a request with unusable output bounds for an
  unknown model answers 404, and for a rate-limited key 429, where
  v0.6.0 answered 400; and a refused request still holds no shared
  capacity and books nothing.
- The gateway's output default is reserved only when a request names
  neither `max_tokens` nor `max_completion_tokens`; v0.6.0 also reserved
  it beside a lone `max_completion_tokens`, which now reserves its own
  cap.
- A stream cut before the provider reported its prompt size is booked, on
  the estimated size, at the row's dearest price tier.
- `logit_bias` and `prompt_cache_key` are forwarded on Chat Completions
  passthrough models; v0.6.0 discarded both.
- On a Chat Completions passthrough row, message `content` must be a
  string, `null` or an array of JSON objects with string `type` fields,
  and only `text`, `image_url` and `refusal` parts are forwarded; any
  other part type, such as `file` or `input_audio`, answers `400 invalid
  message content` before admission. Every part must be an object with
  exactly one lowercase `type` of `text`, `image_url` or `refusal` and
  only the keys that type allows (`text`: `type`, `text`,
  `cache_control`; `image_url`: `type`, `image_url`; `refusal`: `type`,
  `refusal`), each spelled exactly and at most once; anything else, such
  as a part with no `type`, a `file`, `file_id`, `input_audio` or unknown
  key, or a re-cased `Type`, is refused the same way. A part is an image
  exactly when its `type` is `image_url`. Image parts are charged the
  per-image allowance. v0.6.0 forwarded such content unread.
- A `prompt_cache_key` that is not a string is ignored on every row
  instead of failing the request.

### Added

- Model-table fields, all optional: `default_max_tokens`, `sampling`,
  `image_input`, `price_tiers` and `cache_ttl`. See the README's model
  table section.
- Tiered prices: a request whose prompt exceeds a tier's
  `above_input_tokens` — uncached, cache-read and cache-write input
  together — is booked whole at that tier, and admission reserves at the
  dearest tier.
- Images on Anthropic and Responses models whose row declares
  `image_input: true`: `image_url` parts carrying a `data:` URI or an
  `https://` URL. An inline image must be standard base64 in whole groups
  of four, with a media type that is exactly `image/jpeg`, `image/png`,
  `image/gif` or `image/webp`, in lowercase. Any other image is a 400
  naming what is wrong, and a row without `image_input` answers `400
  model <id> does not support image input`.
- Admission charges each image a fixed allowance instead of counting its
  bytes: 4,784 tokens on Anthropic rows and 36,000 on OpenAI rows.
  Inline images still need `MAX_BODY_BYTES` raised to be read at all.
- `cache_ttl: "1h"` asks Anthropic for the one-hour prompt cache on every
  breakpoint.
- `prompt_cache_key` is forwarded to OpenAI as a digest namespaced by the
  gateway key.
- Startup refuses a row whose `default_max_tokens` exceeds
  `MAX_OUTPUT_TOKENS`.
- Built with Go 1.26.9, which fixes the standard-library vulnerabilities
  `govulncheck` reported against 1.26.6 and 1.26.8.

### Upgrading

- A v0.6.0 model table loads unchanged. No configuration or schema
  change.
- The five new row fields are optional. Absent, `default_max_tokens`
  falls back to `DEFAULT_MAX_TOKENS`, `sampling` is `true` on Anthropic
  and Chat Completions rows and `false` on Responses rows,
  `image_input` is `false`, `price_tiers` is one flat price, and
  `cache_ttl` is `"5m"`.
- v0.6.0 ignores those fields silently, so deploy v0.7.0 first and the
  table that uses them second; a table using them under v0.6.0 would
  serve without the limits, tiers and cache lifetime it declares.
- Bodies carrying inline images need `MAX_BODY_BYTES` raised, with the
  reverse proxy's own limit.

## v0.6.0 — 2026-09-25

### BREAKING

- The admin listener refuses to start without `ADMIN_ALLOWED_USERS` and
  `ADMIN_PROXY_SECRET_FILE`. Every admin request, UI included, must carry
  `X-Modelgate-Proxy-Secret` (the file's contents, at least 32 bytes) and
  exactly one identity header whose value is on the allowlist; anything
  else is `403 forbidden`. v0.5.0 trusted any non-empty identity header.
- An exhausted key quota or monthly budget answers `429` with type and code
  `insufficient_quota` and `x-should-retry: false`, replacing `429
  rate_limit_error` with code `quota_exhausted` or `budget_exhausted`. The
  metric and log outcome labels keep the old names.
- A request asking for more output than `MAX_OUTPUT_TOKENS` (default
  128000, `n` × the cap), or with a non-positive `max_tokens`,
  `max_completion_tokens` or `n`, is refused with a 400.
- An unauthenticated request is answered `401` before its body is read;
  v0.5.0 could answer it `400` or `413` first.

### Added

- Model-table limits: `forced_tool_choice: false` and
  `reasoning_efforts: [...]` declare what a model refuses, and modelgate
  answers such a request with a `400 invalid_request_error` naming the
  model and the rule, before it takes a concurrency slot or reserves cost,
  and without booking spend.
  Both fields are optional; absent, behaviour is unchanged.
- `400 invalid_request_error` with code `context_length_exceeded` and
  `param: messages` when the upstream says the conversation does not fit
  the context window.
- Worst-case cost reservations: a request is admitted only while recorded
  spend plus every in-flight reservation stays below the key quota and the
  budget, so parallel requests can no longer run far past a cap. A cap held
  only by requests in flight answers the retryable `429 cap_reserved` with
  `Retry-After: 5`.
- `Retry-After` on per-key rate-limit and concurrency-slot 429s.
- `X-Request-ID` on every public response, adopted from the caller when
  valid; the request log line gains `request_id`, `duration_ms` and
  `version`.
- `MAX_OUTPUT_TOKENS` configuration.
- Prompt caching on by default for Anthropic models: one breakpoint at the
  end of the system prompt (or the last tool) plus the request-level
  automatic breakpoint, five-minute TTL.
- `parallel_tool_calls: false` is translated to Anthropic's
  `disable_parallel_tool_use`; `user` becomes a hashed Anthropic
  `metadata.user_id` and the Responses `user`; `max_completion_tokens` is
  preferred over `max_tokens` on Anthropic models.
- A 5-minute stream idle timeout on every upstream stream, answered as
  `504 timeout`.
- Anthropic upstream errors are logged at WARN, redacted and truncated, as
  OpenAI's already were.
- Store schema versioning through `PRAGMA user_version`; `/ready` now
  commits a real write.
- `modelgate_usage_booking_failures_total`; `modelgate_month_spend_usd`
  and `modelgate_breaker_open` are read at scrape time.
- [docs/compatibility.md](docs/compatibility.md): the per-field contract
  for each upstream.
- CI: a job that rebuilds the admin UI and fails when the committed
  `webui/dist` differs; govulncheck pinned to v1.8.0; the build image
  pinned by digest; Dependabot for Go modules, npm, the Containerfile and
  GitHub Actions.

### Fixed

- A streamed call to a tool that takes no arguments sent `""` as its
  arguments, which a client could not echo back; it now sends `{}`, and
  `""` in history is read as `{}`.
- A stream cut before its final usage event booked almost nothing; it now
  books an estimate, and the whole output cap for calls that can bill
  output they never stream.
- A non-streaming client that hung up booked nothing; the upstream call
  now runs to completion and its usage is booked.
- The month-spend gauge went stale across a month rollover.
- `BUDGET_MONTHLY_USD` accepted `NaN` and `Inf`.
- The Chat Completions passthrough dropped the upstream's `logprobs`.
- The admin UI showed a key with an empty model allowlist as allowed on
  all models; it now shows `none`, since such a key is refused on every
  model.

### Upgrade notes

1. Before upgrading, create the proxy secret
   (`openssl rand -hex 32 > proxy.secret`, mode `0600`), configure the
   reverse proxy to send it as `X-Modelgate-Proxy-Secret` and to strip any
   client copy, and set `ADMIN_PROXY_SECRET_FILE` and
   `ADMIN_ALLOWED_USERS`. Without them v0.6.0 does not start.
2. Clients or alerts that matched `quota_exhausted` or `budget_exhausted`
   in the error body should match `insufficient_quota`; the message still
   says which cap was hit.
3. Back up `DATA_DIR`. The first start stamps a v0.5.0 database as schema
   version 1 and migrates it to version 2 (an added `readiness` table). A
   v0.5.0 binary still opens the migrated database, so a rollback needs no
   restore.
4. Optionally add the model-table limits; for example
   `"forced_tool_choice": false` on `claude-opus-5-5` and
   `"reasoning_efforts": ["none", "low", "medium", "high", "xhigh", "max"]`
   on `gpt-6-sol` and `gpt-6-luna`. v0.5.0 ignores both fields, so the
   same table can roll back.
5. Prompt caching now bills Anthropic cache writes at 1.25× input on
   prefixes long enough to cache; workloads of one-off prompts cost
   slightly more, agent loops less.
6. Give the container a stop timeout above the 30-second drain window.

## v0.5.0 — 2026-09-08

- `reasoning_effort` on Anthropic models is translated to `output_config.effort`
  (`none`/`minimal` → `low`; unknown levels are a 400) instead of rejected.
- Behaviour change on Anthropic models: `temperature` and `top_p` are dropped
  whenever `reasoning_effort` is set. v0.4.0 rejected such a request itself, so
  it never reached the upstream; v0.5.0 translates the effort and sends the
  request without its sampling parameters, because models released after Claude
  Opus 4.6 reject a temperature other than 1.0 and a `top_p` below 0.99.
- `developer` messages fold into the Anthropic `system` prompt.
- A `stop` of `null`, of `""`, or of a list with no non-empty entry is read as
  unset on the two translated paths: Anthropic no longer receives
  `stop_sequences: [""]`, and the Responses upstream accepts such a request
  instead of a 400. The OpenAI Chat Completions path still forwards `stop`
  verbatim, as it does the rest of the request.
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
- CI runs govulncheck.

## v0.4.0 — 2026-08-27

- Admin `GET /api/models` lists the model table (id and provider).
- The admin UI's create form replaces the comma-separated model field with
  an all-models switch and a checkbox list, and shows a read-only models
  panel.
- Spend below one cent is shown to six significant digits instead of
  rounding to zero.
- README example uses a current OpenAI model (`gpt-5.6-terra`) and explains
  `cache_write_usd_per_mtok` on OpenAI models.

## v0.3.1 — 2026-08-27

- The container image is also tagged `latest` on every release.

## v0.3.0 — 2026-08-21

- OpenAI as a second upstream: model-table entries take `provider:
  openai`, requests pass through Chat Completions with the model name
  rewritten, and usage is always requested upstream for billing.
- Startup requires exactly the credential files the table references;
  each provider has its own circuit breaker, and `/ready` checks every
  configured key file.
- `ReadHeaderTimeout` and `IdleTimeout` on all three listeners.
- The gateway's own request deadline no longer opens the circuit breaker.
- The admin listener rejects cross-site browser mutations.
- `modelgate_breaker_open` is labelled by provider.
- An aborted OpenAI stream books a character-count output estimate.
- `cached_tokens` is clamped into `[0, prompt_tokens]`.
- `DEFAULT_MAX_TOKENS` applies to OpenAI models.
- Requests to Anthropic models that name unsupported fields are refused
  instead of silently dropped.
- README quickstart and security-model section.

## v0.2.0 — 2026-08-21

- An upstream 4xx answers `400 invalid_request_error` and no longer opens
  the circuit breaker; client disconnects count as `client_aborted`.
- Streamed usage is booked on every exit path, so an aborted stream
  cannot bypass the budget and quota.
- Request model strings become metric labels only after admission;
  unresolved models count under `model="unknown"`.
- In-stream provider error events count toward the breaker.
- Revocations record the acting identity (`revoked_by`).
- Structured JSON logs for startup, admin mutations and booking failures.
- Spend and key-count gauges are initialised at startup.
- Empty-content messages are refused.
- `stream_options.include_usage` emits the final usage chunk.

## v0.1.0 — 2026-08-21

- OpenAI-compatible `POST /v1/chat/completions` and `GET /v1/models` in
  front of the Anthropic Messages API, streaming included.
- `mg_` client keys stored as SHA-256 digests, with a model allowlist, a
  monthly USD quota and an expiry.
- Per-key quotas and a hard global monthly budget, priced from a
  fail-closed model table.
- Per-key rate limit, global concurrency cap, request deadline, and an
  Anthropic client with bounded retries and a circuit breaker.
- Admin JSON API and an embedded admin UI behind a trusted identity
  header; Prometheus metrics and `/ready` on a separate listener.
- SQLite store; a static `scratch` container image published to GHCR on
  version tags.
