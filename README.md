# modelgate

A single-binary, OpenAI-compatible LLM gateway in front of Anthropic and
OpenAI. It issues its own revocable client keys and meters spend per key
and globally against hard monthly ceilings, so one set of provider
credentials can serve many clients without being handed to any of them.
Requests for Anthropic models are translated to the Anthropic Messages
API (streaming included); OpenAI models pass through, with modelgate
supplying the model name, spend accounting, and error responses. A
key-management page is embedded in the binary.

## Quickstart

```sh
printf '%s' "$ANTHROPIC_API_KEY" > /tmp/anthropic.key
cat > /tmp/models.json <<'EOF'
{"models": {"claude-sonnet-5": {
  "provider_model": "claude-sonnet-5",
  "input_usd_per_mtok": 3.0, "output_usd_per_mtok": 15.0,
  "cache_read_usd_per_mtok": 0.30, "cache_write_usd_per_mtok": 3.75}}}
EOF

PUBLIC_ADDR=127.0.0.1:8080 ADMIN_ADDR=127.0.0.1:8081 DATA_DIR=/tmp/modelgate ANTHROPIC_API_KEY_FILE=/tmp/anthropic.key MODELS_CONFIG_FILE=/tmp/models.json BUDGET_MONTHLY_USD=50 go run ./cmd/modelgate
```

Create a key through the admin API (the header is normally injected by
your reverse proxy — see Security model):

```sh
curl -s -X POST 127.0.0.1:8081/api/keys   -H 'Remote-User: you' -H 'Content-Type: application/json'   -d '{"label": "laptop"}'
```

Then use the returned `full_key` like any OpenAI endpoint:

```sh
curl -s 127.0.0.1:8080/v1/chat/completions   -H "Authorization: Bearer mg_..."   -d '{"model": "claude-sonnet-5", "messages": [{"role": "user", "content": "hi"}]}'
```

A container image is published as `ghcr.io/snowballsh/modelgate` on each
tag. The pre-built admin UI is checked in under `webui/dist`, so
`go build ./cmd/modelgate` needs no Node toolchain.

## Listeners

| Listener | Serves | Auth |
|---|---|---|
| `PUBLIC_ADDR` | `POST /v1/chat/completions`, `GET /v1/models` | `Authorization: Bearer` modelgate key |
| `ADMIN_ADDR` | Admin JSON API + embedded admin UI | Non-empty trusted-proxy identity header |
| `METRICS_ADDR` | Prometheus exposition, `/ready` | None (keep it loopback) |

The listeners share no routes.

## Security model

modelgate does not authenticate admins itself. It trusts whatever value
arrives in the `ADMIN_IDENTITY_HEADER` header (default `Remote-User`)
and rejects only its absence — so the admin listener must be reachable
solely through a reverse proxy that authenticates the user, sets that
header, and strips any client-supplied copy. Anyone who can reach
`ADMIN_ADDR` directly can mint keys. The admin UI is the admin
listener's root page; `/api/*` on the same listener is the JSON API it
uses, and cross-site browser mutations are rejected. Keep `METRICS_ADDR`
on loopback: it carries no auth at all.

## Configuration

All configuration is environment variables. The service refuses to start
when a required value is missing or unreadable — there is no partial
startup.

| Variable | Meaning | When missing |
|---|---|---|
| `PUBLIC_ADDR`, `ADMIN_ADDR` | Listener addresses | refuse to start |
| `METRICS_ADDR` | Metrics/readiness address | metrics disabled |
| `DATA_DIR` | SQLite location | refuse to start |
| `ANTHROPIC_API_KEY_FILE` | Anthropic credential file | refuse to start if the table uses Anthropic |
| `ANTHROPIC_BASE_URL` | Anthropic origin | Anthropic production |
| `OPENAI_API_KEY_FILE` | OpenAI credential file | refuse to start if the table uses OpenAI |
| `OPENAI_BASE_URL` | OpenAI origin | OpenAI production |
| `MODELS_CONFIG_FILE` | Model table (below) | refuse to start |
| `BUDGET_MONTHLY_USD` | Global hard ceiling | refuse to start |
| `ADMIN_IDENTITY_HEADER` | Trusted identity header name | default `Remote-User` |
| `DEFAULT_MAX_TOKENS` | When the request omits `max_tokens` | default 4096 |
| `MAX_BODY_BYTES` | Public request body limit | default 1 MiB |
| `RATE_LIMIT_PER_KEY_RPM` | Per-key request rate | default 60 |
| `MAX_CONCURRENT_REQUESTS` | Global in-flight cap | default 8 |
| `REQUEST_DEADLINE` | Hard per-request deadline | default 10m |

## Model table

`MODELS_CONFIG_FILE` maps public model IDs to a provider (`anthropic`,
the default, or `openai`), the provider's model name, and USD per-MTok
prices. A request naming a model absent from the table, or listed
without all four prices, is refused — an unpriced request can never run.
OpenAI bills no separate cache write, so for OpenAI models set
`cache_write_usd_per_mtok` to the input price; the gateway never
multiplies it by a nonzero count.
Each provider referenced by the table must have its credential file
configured, and each gets its own circuit breaker.

```json
{
  "models": {
    "claude-sonnet-5": {
      "provider_model": "claude-sonnet-5",
      "input_usd_per_mtok": 3.0,
      "output_usd_per_mtok": 15.0,
      "cache_read_usd_per_mtok": 0.30,
      "cache_write_usd_per_mtok": 3.75
    },
    "gpt-5.6-terra": {
      "provider": "openai",
      "provider_model": "gpt-5.6-terra",
      "input_usd_per_mtok": 2.0,
      "output_usd_per_mtok": 12.0,
      "cache_read_usd_per_mtok": 0.20,
      "cache_write_usd_per_mtok": 2.0
    }
  }
}
```

## Keys

Keys look like `mg_<id>_<secret>`. Only the SHA-256 of the secret is
stored; the full key is shown exactly once, by the create call. Rotation
is create-new then revoke-old. Each key may carry a model allow-list, a
monthly USD quota, and an expiry.

## Spend ceilings

Every completed request's token usage is priced from the model table and
accumulated per key and globally by UTC calendar month. Once
month-to-date spend reaches `BUDGET_MONTHLY_USD`, new requests get
`budget_exhausted`; a key that reaches its own quota gets
`quota_exhausted`. In-flight streams are allowed to finish, so overshoot
is bounded by `MAX_CONCURRENT_REQUESTS` requests of at most `max_tokens`
each.

## Development

```
go test -race ./...
```

Integration tests (`-tags integration`) run the built server against a
fake Anthropic upstream and the official `openai-go` client.

## License

MIT — see [LICENSE](LICENSE).
