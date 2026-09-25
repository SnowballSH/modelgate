# modelgate

A single-binary, OpenAI-compatible LLM gateway in front of Anthropic and
OpenAI. It issues its own revocable client keys and meters spend per key
and globally against hard monthly ceilings, so one set of provider
credentials can serve many clients without being handed to any of them.
Requests for Anthropic models are translated to the Anthropic Messages
API (streaming included); OpenAI models go to Chat Completions, or to the
Responses API when the model table says so, with modelgate supplying the
model name, spend accounting, and error responses. A key-management page
is embedded in the binary.

The per-field contract for every upstream — what is translated, passed
through, rejected or dropped — is in
[docs/compatibility.md](docs/compatibility.md). Release history is in
[CHANGELOG.md](CHANGELOG.md).

## Quickstart

```sh
mkdir -p /tmp/modelgate
printf '%s' "$ANTHROPIC_API_KEY" > /tmp/modelgate/anthropic.key
openssl rand -hex 32 > /tmp/modelgate/proxy.secret
cat > /tmp/modelgate/models.json <<'EOF'
{"models": {"claude-sonnet-5": {
  "provider_model": "claude-sonnet-5",
  "input_usd_per_mtok": 2.0, "output_usd_per_mtok": 10.0,
  "cache_read_usd_per_mtok": 0.20, "cache_write_usd_per_mtok": 2.50}}}
EOF

PUBLIC_ADDR=127.0.0.1:8080 ADMIN_ADDR=127.0.0.1:8081 METRICS_ADDR=127.0.0.1:8082 \
DATA_DIR=/tmp/modelgate/data \
ANTHROPIC_API_KEY_FILE=/tmp/modelgate/anthropic.key \
MODELS_CONFIG_FILE=/tmp/modelgate/models.json \
BUDGET_MONTHLY_USD=50 \
ADMIN_ALLOWED_USERS=you \
ADMIN_PROXY_SECRET_FILE=/tmp/modelgate/proxy.secret \
go run ./cmd/modelgate
```

Create a key through the admin API. In production a reverse proxy adds
both headers after authenticating you (see [Admin boundary](#admin-boundary));
here you add them yourself:

```sh
curl -s -X POST 127.0.0.1:8081/api/keys \
  -H "X-Modelgate-Proxy-Secret: $(cat /tmp/modelgate/proxy.secret)" \
  -H 'Remote-User: you' -H 'Content-Type: application/json' \
  -d '{"label": "laptop"}'
```

Then use the returned `full_key` like any OpenAI endpoint:

```sh
curl -s 127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer mg_..." \
  -d '{"model": "claude-sonnet-5", "messages": [{"role": "user", "content": "hi"}]}'
```

A container image is published as `ghcr.io/snowballsh/modelgate` on each
tag, tagged with the version and `latest`, and built with the tag as the
version `modelgate --version` prints (an unreleased build prints `dev`).
The pre-built admin UI is checked in under `webui/dist`, so
`go build ./cmd/modelgate` needs no Node toolchain.

## Listeners

| Listener | Serves | Auth |
|---|---|---|
| `PUBLIC_ADDR` | `POST /v1/chat/completions`, `GET /v1/models` | `Authorization: Bearer` modelgate key |
| `ADMIN_ADDR` | Admin JSON API under `/api/`, the embedded admin UI everywhere else | Proxy secret and an allowlisted identity |
| `METRICS_ADDR` | `GET /metrics` (Prometheus), `GET /ready` | None: keep it on loopback |

The listeners share no routes. Any other path answers
`404 {"error":{"type":"invalid_request_error","code":"not_found"}}`.

## Configuration

All configuration is environment variables. The service refuses to start
when a required value is missing, malformed or unreadable — there is no
partial startup — and it refuses before any port opens.

| Variable | Meaning | Default and validation |
|---|---|---|
| `PUBLIC_ADDR` | Public listener address | Required |
| `ADMIN_ADDR` | Admin listener address | Required |
| `METRICS_ADDR` | Metrics and readiness listener address | Unset disables the listener |
| `DATA_DIR` | Directory for the SQLite store; created `0700` if missing | Required, must be writable |
| `MODELS_CONFIG_FILE` | The [model table](#model-table) | Required, must load |
| `BUDGET_MONTHLY_USD` | Global hard spend ceiling per UTC calendar month | Required; a positive finite number (`0`, negatives, `NaN` and `Inf` are refused) |
| `ADMIN_ALLOWED_USERS` | Comma-separated identities allowed on the admin listener, matched exactly; blanks are dropped | Required, at least one identity |
| `ADMIN_PROXY_SECRET_FILE` | File holding the secret the reverse proxy sends as `X-Modelgate-Proxy-Secret`; trailing CR/LF characters are trimmed | Required; the secret must be at least 32 bytes |
| `ADMIN_IDENTITY_HEADER` | Header carrying the authenticated identity | `Remote-User` |
| `ANTHROPIC_API_KEY_FILE` | Anthropic credential file | Required when the table has an Anthropic model; must be non-empty |
| `ANTHROPIC_BASE_URL` | Anthropic origin | `https://api.anthropic.com` |
| `OPENAI_API_KEY_FILE` | OpenAI credential file | Required when the table has an OpenAI model; must be non-empty |
| `OPENAI_BASE_URL` | OpenAI origin | `https://api.openai.com` |
| `DEFAULT_MAX_TOKENS` | Output cap sent when a request names none | `4096`; a positive integer no larger than `MAX_OUTPUT_TOKENS` |
| `MAX_OUTPUT_TOKENS` | Largest output (`n` × the cap) a request may ask for; larger requests get a 400 | `128000`; a positive integer |
| `MAX_BODY_BYTES` | Public request body limit | `1048576` (1 MiB); a positive integer |
| `RATE_LIMIT_PER_KEY_RPM` | Per-key request rate (token bucket, burst equal to the rate) | `60`; a positive integer |
| `MAX_CONCURRENT_REQUESTS` | Global cap on admitted requests in flight | `8`; a positive integer |
| `REQUEST_DEADLINE` | Hard per-request deadline, streams included (Go duration) | `10m`; positive |

Fixed timings: a public request body must arrive within 30 seconds of the
headers, request headers within 10 seconds, idle keep-alive connections
close after 2 minutes, and a stream that sends nothing for 5 minutes is
aborted with `504 timeout`.

## Model table

`MODELS_CONFIG_FILE` maps public model IDs to the provider that serves
them, the provider's own model name, USD prices per million tokens, and
optionally the request features the provider refuses for that model. A
request naming a model absent from the table is refused, and a table
entry without all four prices refuses the whole table: an unpriced
request can never run.

| Field | Meaning |
|---|---|
| `provider` | `anthropic` (the default) or `openai`. |
| `upstream_api` | Which OpenAI API serves the model: `chat_completions` (the default) or `responses`. Valid on `openai` models only; an unknown value, or any value on an Anthropic model, refuses the table. |
| `provider_model` | The model name sent upstream. Required. |
| `input_usd_per_mtok`, `output_usd_per_mtok`, `cache_read_usd_per_mtok`, `cache_write_usd_per_mtok` | All four required, all positive. |
| `forced_tool_choice` | Optional boolean, default `true`. `false` declares that the model refuses forced tool use, and modelgate answers a request whose `tool_choice` is `"required"`, a named function or custom tool, or an `allowed_tools` set in `required` mode with a 400 of its own. |
| `reasoning_efforts` | Optional list of the `reasoning_effort` levels the model accepts, compared against the request's `reasoning_effort` before translation and drawn from `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max` (exact, lowercase). A request naming any other level gets a 400 of its own. Omitted or `null` accepts every level; an empty list or an unknown level refuses the table. |

The limits are checked after the key's model allowlist and before the
request takes a concurrency slot or reserves any cost, so a refused
request holds no shared capacity, books nothing and never reaches the
provider. The 400 names the model and the rule:

```json
{"error": {"type": "invalid_request_error", "code": "invalid_request_error",
  "message": "model claude-opus-5-5 does not support forced tool_choice; use \"auto\""}}
```

Fields the running build does not know are ignored, so a table written
for a newer release loads in an older one (without the newer checks),
and a v0.5.0 table loads unchanged in v0.6.0.

OpenAI bills no separate cache write, so for OpenAI models set
`cache_write_usd_per_mtok` to the input price; the gateway never
multiplies it by a nonzero count. Each provider referenced by the table
must have its credential file configured, and each gets its own circuit
breaker.

A current table, at the providers' published prices:

```json
{
  "models": {
    "claude-opus-5-5": {
      "provider_model": "claude-opus-5-5",
      "forced_tool_choice": false,
      "input_usd_per_mtok": 4.0,
      "output_usd_per_mtok": 20.0,
      "cache_read_usd_per_mtok": 0.2,
      "cache_write_usd_per_mtok": 5.0
    },
    "claude-sonnet-5": {
      "provider_model": "claude-sonnet-5",
      "input_usd_per_mtok": 2.0,
      "output_usd_per_mtok": 10.0,
      "cache_read_usd_per_mtok": 0.2,
      "cache_write_usd_per_mtok": 2.5
    },
    "claude-haiku-4-5": {
      "provider_model": "claude-haiku-4-5",
      "input_usd_per_mtok": 1.0,
      "output_usd_per_mtok": 5.0,
      "cache_read_usd_per_mtok": 0.1,
      "cache_write_usd_per_mtok": 1.25
    },
    "gpt-6-sol": {
      "provider": "openai",
      "upstream_api": "responses",
      "provider_model": "gpt-6-sol",
      "reasoning_efforts": ["none", "low", "medium", "high", "xhigh", "max"],
      "input_usd_per_mtok": 2.0,
      "output_usd_per_mtok": 10.0,
      "cache_read_usd_per_mtok": 0.2,
      "cache_write_usd_per_mtok": 2.0
    },
    "gpt-5.6-terra": {
      "provider": "openai",
      "upstream_api": "responses",
      "provider_model": "gpt-5.6-terra",
      "input_usd_per_mtok": 2.0,
      "output_usd_per_mtok": 12.0,
      "cache_read_usd_per_mtok": 0.2,
      "cache_write_usd_per_mtok": 2.0
    },
    "gpt-6-luna": {
      "provider": "openai",
      "upstream_api": "responses",
      "provider_model": "gpt-6-luna",
      "reasoning_efforts": ["none", "low", "medium", "high", "xhigh", "max"],
      "input_usd_per_mtok": 0.1,
      "output_usd_per_mtok": 0.5,
      "cache_read_usd_per_mtok": 0.01,
      "cache_write_usd_per_mtok": 0.1
    }
  }
}
```

Claude Opus 5.5 and Claude Fable 5.1 refuse forced tool use; GPT-6 Sol
and Luna have no `minimal` effort. Anthropic cache writes are the
five-minute rate, 1.25× input. A table cannot express tiered prices
(GPT-6 bills prompts over 272K input tokens at a higher rate), so such a
prompt is booked at the table's rate.

## Request parameters on Anthropic models

| Request field | Handling |
|---|---|
| `reasoning_effort` | Translated to `output_config.effort`. `none` and `minimal` map to `low`; `low`, `medium`, `high`, `xhigh` and `max` pass through; anything else is a 400. Omitting it leaves the model's default (`medium` on Claude Opus 5.5, `high` on most others). |
| `temperature`, `top_p` | Forwarded, unless `reasoning_effort` is set: models that accept effort reject a temperature other than 1.0 and a `top_p` below 0.99, so effort displaces both. |
| `stop` | Forwarded as `stop_sequences`. A `null`, an empty string, and a list with no non-empty entry all mean unset. |
| `system`, `developer` messages | Folded into the `system` prompt, which carries a prompt-cache breakpoint. |
| `response_format`, `frequency_penalty`, `presence_penalty`, `seed`, `logprobs`, `top_logprobs`, `n` other than 1 | Rejected with a 400. |

Anthropic's list of effort-supporting models does not name
`claude-haiku-4-5`, so a `reasoning_effort` on that model is refused
upstream: the client gets `400 invalid_request_error` with modelgate's
fixed message ("the provider rejected the translated request"), and the
upstream's own reason goes to the log, never to the client. Top-level
effort shapes the rendered prompt, so changing it between requests does
not reuse Anthropic's cached prefixes from earlier turns.

## Request parameters on the Responses upstream

| Request field | Handling |
|---|---|
| `reasoning_effort` | Sent as `reasoning.effort` unchanged; anything outside `none`, `minimal`, `low`, `medium`, `high`, `xhigh` and `max` is a 400. Declare a model's accepted levels with `reasoning_efforts` to have modelgate refuse the rest; otherwise an unsupported level is refused upstream. |
| `temperature`, `top_p` | Dropped: this upstream serves reasoning models, which accept only the default of either. |
| `max_completion_tokens`, `max_tokens` | Sent as `max_output_tokens`, which caps reasoning and visible output together. |
| `system`, `developer` messages | Folded into `instructions`. |
| `response_format` | Translated to `text.format`. |
| `stop` | Rejected with a 400 when it names a stop sequence; `null`, `""` and a list with no non-empty entry are accepted as unset. |
| `frequency_penalty`, `presence_penalty`, `seed`, `logprobs`, `top_logprobs`, `n` other than 1 | Rejected with a 400. |

Requests are sent with `store: false`, so nothing is retained upstream.
The known cost of that: a Chat Completions history carries no reasoning
items, so an assistant tool call goes back with its `call_id` alone and
the model re-reasons after every tool result — reasoning it bills for and
`max_output_tokens` caps alongside the answer.

## Public API

`POST /v1/chat/completions` and `GET /v1/models`, OpenAI's wire format.
Authenticate with `Authorization: Bearer mg_<id>_<secret>`; a missing,
malformed, unknown, revoked or expired key gets `401 invalid_api_key`,
and the body is not read until the key checks out. `GET /v1/models` lists
the table's models the key may use.

A request is admitted in this order: the key, the body (size and read
deadline), the JSON and its output bounds, the key's rate limit, the
model and the key's allowlist, the model's declared limits, a
concurrency slot, a reservation of the request's worst-case cost against
the key quota and the monthly budget, then the provider's circuit
breaker.

Every response carries `X-Request-ID`: the caller's own value when it is
1–128 characters of `A-Z a-z 0-9 - _ . :`, otherwise a minted
`req_<32 hex>`. The request log line carries the same id.

A 429 that waiting will clear carries `Retry-After` in whole seconds: the
time until the key's rate bucket refills, `1` when every concurrency slot
is taken, and `5` for `cap_reserved`. A 429 for an exhausted quota or
budget carries `x-should-retry: false` instead, which the OpenAI SDKs
honour; only the next month or an operator clears it. An upstream 429 is
answered as `rate_limited` without `Retry-After`.

### Errors

Every error is OpenAI's shape,
`{"error": {"message": "...", "type": "...", "code": "..."}}`. The message
is modelgate's own: an upstream's error body never reaches the client.

| Status | `type` | `code` | When |
|---|---|---|---|
| 400 | `invalid_request_error` | `invalid_request_error` | Invalid JSON, a field modelgate or the upstream refuses, a non-positive `max_tokens` or `n`, output above `MAX_OUTPUT_TOKENS`, a declared model limit, or any other upstream 4xx |
| 400 | `invalid_request_error` | `context_length_exceeded` | The upstream says the conversation does not fit the context window; `param` is `messages` |
| 401 | `authentication_error` | `invalid_api_key` | The key is missing, unknown, revoked or expired |
| 404 | `invalid_request_error` | `model_not_found` | The model is not in the table, or not on the key's allowlist |
| 404 | `invalid_request_error` | `not_found` | Unknown route |
| 408 | `invalid_request_error` | `request_timeout` | The body did not arrive within 30 seconds |
| 413 | `invalid_request_error` | `request_too_large` | The body exceeds `MAX_BODY_BYTES` |
| 429 | `rate_limit_error` | `rate_limited` | The key's rate limit, every concurrency slot taken, or an upstream 429 |
| 429 | `rate_limit_error` | `cap_reserved` | The key quota or the budget is not spent but is fully held by requests in flight; retry after `Retry-After` |
| 429 | `insufficient_quota` | `insufficient_quota` | The key's monthly quota or the global monthly budget is spent; the message says which |
| 500 | `api_error` | `api_error` | The store failed |
| 502 | `api_error` | `provider_auth_error` | The upstream refused modelgate's credential |
| 503 | `api_error` | `provider_unavailable` | Upstream 5xx or transport failure after retries, the provider's circuit is open, or a Responses run reported `failed` |
| 504 | `api_error` | `timeout` | The request deadline or the stream idle timeout expired |

After a stream has started, a failure arrives as one in-band SSE event
carrying the same error body, and the stream ends without `[DONE]`.

Upstream calls are retried up to three attempts with exponential backoff
on 429, 5xx and transport failures; a stream only while nothing has
reached the client. Five upstream failures without a success between
them (credential refusals, 429s, 5xx or transport errors; other 4xx and
timeouts do not count) open that provider's circuit for 30 seconds.

## Admin API

All routes are on `ADMIN_ADDR`, JSON in and out.

| Route | Body | Answer |
|---|---|---|
| `GET /api/keys` | | `200 {"keys": [key, ...]}` |
| `POST /api/keys` | `{"label": "ci", "models": ["claude-sonnet-5"], "quota_usd": 10, "expires_at": "2026-12-31T23:59:59Z"}`; only `label` is required | `201 {"key": key, "full_key": "mg_..."}` |
| `POST /api/keys/{id}/revoke` | | `200 {"key": key}`; `404` for an unknown id |
| `GET /api/usage` | | `200 {"month", "budget_usd", "spend_usd", "keys": [{"id", "label", "spend_usd", "quota_usd"}]}` |
| `GET /api/models` | | `200 {"models": [{"id", "provider"}]}` |

A key object carries `id`, `prefix`, `label`, `models`, `quota_usd`,
`expires_at`, `revoked_at`, `revoked_by`, `last_used_at`, `created_at`,
`created_by` and `month_spend_usd`. `models` is the key's allowlist:
`null` (or omitted at creation) allows every model in the table, a list
allows exactly those models, and an empty list `[]` allows none, so every
request the key makes is refused. Every listed model must be in the
table; `quota_usd` must be positive; `expires_at` must be RFC 3339 and in
the future. A refused creation is a 400 with code `invalid_request_error`
or, for a model not in the table, `unknown_model`.

### Admin boundary

modelgate does not log admins in itself; a reverse proxy in front of
`ADMIN_ADDR` does, and modelgate authenticates the proxy. Every admin
request — API and UI alike — must carry:

- `X-Modelgate-Proxy-Secret` exactly once, equal to the contents of
  `ADMIN_PROXY_SECRET_FILE` (compared in constant time), and
- the `ADMIN_IDENTITY_HEADER` header exactly once, with a value listed in
  `ADMIN_ALLOWED_USERS`.

The proxy must authenticate the user, set both headers, and strip any
client-supplied copies. A reachable port proves nothing: any process that
can reach `ADMIN_ADDR` but lacks the secret is refused, and so is a
proxied user who is not on the allowlist.

Every refusal is the same
`403 {"error":{"message":"admin access denied","type":"permission_error","code":"forbidden"}}`:
a missing, wrong or duplicated secret; a missing, duplicated or
unlisted identity. The WARN log line `admin request refused` records only
`reason` (`proxy_secret` or `identity`) and `remote_addr`, never a
presented value. A `POST` carrying a `Sec-Fetch-Site` other than
`same-origin` or `none` (a missing header is allowed, as from curl), or a
`Content-Type` other than JSON, is refused as `403 cross_site_request`,
because the proxy may authenticate with a cookie a hostile page could
ride.

Startup fails if either variable is unset, the secret file is unreadable,
or the secret is shorter than 32 bytes. Keep `METRICS_ADDR` on loopback:
it has no authentication at all.

## Keys

Keys look like `mg_<id>_<secret>`. Only the SHA-256 of the secret is
stored; the full key is shown exactly once, by the create call. Rotation
is create-new then revoke-old. Each key may carry a model allowlist, a
monthly USD quota, and an expiry.

## Spend ceilings

Every completed request's token usage is priced from the model table and
accumulated per key and globally by UTC calendar month. Reasoning tokens
are part of the output count and are billed as output tokens; when the
upstream reports them separately they are passed on in
`usage.completion_tokens_details.reasoning_tokens`.

Admission reserves each request's worst-case cost — the body's bytes
plus 2,048 tokens of overhead at the dearest input-side price, plus the
largest output the request allows at the output price — and holds it
until the real usage is booked. A request is admitted only while
recorded spend plus every reservation stays below the key quota and the
budget, so concurrent requests can overshoot a cap by at most one
request's bound. Recorded spend at a cap answers `insufficient_quota`; a
cap held only by reservations answers the retryable `cap_reserved`.

A stream cut before its final usage event is booked from an estimate:
output of at least one token per four streamed characters, and the whole
output cap when the call can bill output it never streams (every
Responses call, and any call with a `reasoning_effort`). A non-streaming
call keeps running when the client hangs up, because the provider bills
it either way, and its real usage is booked.

## Metrics

`GET /metrics` on `METRICS_ADDR`:

| Metric | Type | Labels |
|---|---|---|
| `modelgate_requests_total` | counter | `outcome` (`success`, `client_aborted`, or an error code from the table above, with `quota_exhausted`/`budget_exhausted` for the two caps), `model` (`unknown` until admission resolves it) |
| `modelgate_request_duration_seconds` | histogram | `model` |
| `modelgate_tokens_total` | counter | `direction` (`input`, `output`, `cache_read`, `cache_write`), `model` |
| `modelgate_provider_errors_total` | counter | `kind` (`auth`, `rate_limited`, `timeout`, `rejected`, `unavailable`) |
| `modelgate_usage_booking_failures_total` | counter | none; the provider billed spend the quota and budget do not count |
| `modelgate_in_flight` | gauge | none; provider calls in flight |
| `modelgate_keys` | gauge | none |
| `modelgate_budget_usd` | gauge | none |
| `modelgate_month_spend_usd` | gauge, read at scrape | none; `NaN` when the store read fails |
| `modelgate_breaker_open` | gauge, read at scrape | `provider` |

`GET /ready` answers `200 ready` when the store answers a ping, commits a
write, and every configured provider key file is readable and non-empty;
otherwise `503` with the reason.

## Logs

JSON `slog` records on stderr.

Every request through the public listener logs exactly one record, at INFO,
when its outcome is known — pre-admission rejections included:

| Field | Value |
|---|---|
| `msg` | `request` |
| `request_id` | the `X-Request-ID` the response carries |
| `key_id` | the key's id, empty when authentication failed |
| `model` | the resolved model id, `unknown` until admission resolves it |
| `upstream` | `chat_completions` or `responses` on OpenAI models; empty on Anthropic models and when admission failed |
| `reasoning_effort` | the canonical effort, empty when the request named none or one outside the vocabulary |
| `stream` | whether the client asked for a stream |
| `status` | the outcome, the same label the request metric carries |
| `duration_ms`, `version` | elapsed time and the build's version |

No field carries a client-supplied string: `model` is compared against the
model table and `reasoning_effort` against the accepted vocabulary before
either is logged.

When an upstream answers 4xx or 5xx its error is logged at WARN
(`anthropic upstream rejected the request`, or `openai upstream rejected
the request`) with the status and the upstream's message, redacted of
credential-shaped text and truncated to 512 bytes. A 401 or 403 logs no
message, since those bodies can quote the refused key. Request bodies are
never logged.

## Deployment notes

**Graceful drain.** On `SIGTERM` or `SIGINT` modelgate stops accepting
connections and waits up to 30 seconds for in-flight requests, then
exits. Give the container a stop timeout longer than that (for example
`podman run --stop-timeout 40`, or `StopTimeout=40` in a Quadlet) — the
common default of 10 seconds kills the process mid-drain. A stream still
running when the drain window ends is cut, and its usage may go unbooked.

**Schema versioning.** The store records its schema version in SQLite's
`PRAGMA user_version` and migrates forward at startup, one transaction
per step. v0.6.0 is schema version 2; it stamps a v0.5.0 database as
version 1 without altering it, then adds version 2. A database stamped by
a newer release is refused rather than opened. v0.5.0 ignores the
version and the added table, so rolling back from v0.6.0 to v0.5.0 keeps
working; back up `DATA_DIR` before any upgrade regardless.

**Single instance.** Reservations, rate buckets and circuit breakers are
in memory. Run one instance per data directory.

## Development

```sh
go test -race ./...
go test -race -tags integration ./internal/integration/...
```

Integration tests run the built server against fake upstreams and the
official `openai-go` client. After changing `webui/src`, rebuild the UI
with `npm ci && npm run build` in `webui/` and commit `webui/dist`; CI
rebuilds it and fails if any file differs, appears or disappears.

## License

MIT — see [LICENSE](LICENSE).
