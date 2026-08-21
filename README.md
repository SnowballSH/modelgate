# modelgate

A single-binary, OpenAI-compatible LLM gateway in front of the Anthropic
Messages API. modelgate issues its own revocable client keys, meters spend
per key and globally against hard monthly ceilings, and ships an embedded
key-management page — so one provider credential can serve many clients
without ever being handed to them.

## Listeners

| Listener | Serves | Auth |
|---|---|---|
| `PUBLIC_ADDR` | `POST /v1/chat/completions`, `GET /v1/models` | `Authorization: Bearer` modelgate key |
| `ADMIN_ADDR` | Admin JSON API + embedded admin UI | Non-empty trusted-proxy identity header |
| `METRICS_ADDR` | Prometheus exposition, `/ready` | None (keep it loopback) |

The listeners share no routes. Run each behind your own reverse proxy;
modelgate trusts the identity header on the admin listener, so only a
trusted proxy may reach it.

## Configuration

All configuration is environment variables. The service refuses to start
when a required value is missing or unreadable — there is no partial
startup.

| Variable | Meaning | When missing |
|---|---|---|
| `PUBLIC_ADDR`, `ADMIN_ADDR` | Listener addresses | refuse to start |
| `METRICS_ADDR` | Metrics/readiness address | metrics disabled |
| `DATA_DIR` | SQLite location | refuse to start |
| `ANTHROPIC_API_KEY_FILE` | Provider credential file | refuse to start |
| `ANTHROPIC_BASE_URL` | Provider origin | Anthropic production |
| `MODELS_CONFIG_FILE` | Model table (below) | refuse to start |
| `BUDGET_MONTHLY_USD` | Global hard ceiling | refuse to start |
| `ADMIN_IDENTITY_HEADER` | Trusted identity header name | default `Remote-User` |
| `DEFAULT_MAX_TOKENS` | When the request omits `max_tokens` | default 4096 |
| `MAX_BODY_BYTES` | Public request body limit | default 1 MiB |
| `RATE_LIMIT_PER_KEY_RPM` | Per-key request rate | default 60 |
| `MAX_CONCURRENT_REQUESTS` | Global in-flight cap | default 8 |
| `REQUEST_DEADLINE` | Hard per-request deadline | default 10m |

## Model table

`MODELS_CONFIG_FILE` maps public model IDs to the provider model and USD
per-MTok prices. A request naming a model absent from the table — or
present without complete pricing — is refused. Cost accounting fails
closed by construction: nothing unpriced can run.

```json
{
  "models": {
    "claude-sonnet-5": {
      "provider_model": "claude-sonnet-5",
      "input_usd_per_mtok": 3.0,
      "output_usd_per_mtok": 15.0,
      "cache_read_usd_per_mtok": 0.30,
      "cache_write_usd_per_mtok": 3.75
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
accumulated per key and globally by UTC calendar month. At
`BUDGET_MONTHLY_USD` new requests receive `budget_exhausted`; per-key
quotas trip `quota_exhausted` identically. In-flight streams finish, so
overshoot is bounded by `MAX_CONCURRENT_REQUESTS × max_tokens`.

## Development

```
go test -race ./...
```

Integration tests (`-tags integration`) run the built server against a
fake Anthropic upstream and the official `openai-go` client.
