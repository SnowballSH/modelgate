# Anthropic `output_config.effort`, and the sampling parameters it displaces

Recorded 2026-09-07 from vendor documentation.
**Live run: PENDING — operator ceremony** (see below).

B2 translates the OpenAI `reasoning_effort` into the Anthropic Messages
API's effort control and builds a golden suite on the field name and the
enum. A wrong name or a wrong level makes those goldens self-consistent
and wrong — they would pass every test in Part B and fail live. This
record is the contract they are checked against.

## Documented contract

The field is `output_config.effort`, top level on the Messages request,
generally available with no beta header:

> Set `output_config.effort` on the request.

The levels are `low`, `medium`, `high`, `xhigh` and `max`, and `high` is
the default:

> By default, Claude uses high effort, spending as many tokens as needed
> for excellent results.

> Setting `effort` to `"high"` produces exactly the same behavior as
> omitting the `effort` parameter entirely.

— Anthropic, *Effort*,
<https://platform.claude.com/docs/en/build-with-claude/effort> (read
2026-09-07).

That page's compatibility header lists the models that accept effort:
`claude-fable-5-1`, `claude-mythos-5-1`, `claude-fable-5`,
`claude-mythos-5`, `claude-mythos-preview`, `claude-opus-5`,
`claude-opus-4-8`, `claude-opus-4-7`, `claude-opus-4-6`,
`claude-opus-4-5-20251101`, `claude-sonnet-5`, `claude-sonnet-4-6`. Two
consequences for the farm: `claude-opus-5` — the `iris` model — is on the
list and is documented to support all five levels, including `xhigh` and
`max`; and `claude-haiku-4-5` is *not* on it, so an effort level on a
Haiku row is refused upstream and modelgate answers with the provider's
error.

Not every model that supports `max` supports `xhigh`, so the level a
model accepts is a per-model fact, not a property of the enum.

`adaptive` is the value to probe as the negative case, because the same
page says it is not an effort level at all:

> Don't pass `adaptive` as an `effort` value: `adaptive` is a thinking
> mode, not an effort level.

Effort also interacts with the prompt cache — the operator-facing note
B2's README section carries:

> Because top-level effort shapes the rendered prompt, changing it
> between requests doesn't preserve cached prefixes from earlier turns.

## Temperature and `top_p` are rejected on these models

The same models that take `effort` refuse the sampling parameters
`translate.ToAnthropic` currently forwards unconditionally. From the
Messages API reference
(<https://platform.claude.com/docs/en/api/messages>, read 2026-09-07):

> **Deprecated**: Deprecated. Models released after Claude Opus 4.6 do
> not support setting temperature. A value of 1.0 of will be accepted for
> backwards compatibility, all other values will be rejected with a 400
> error.

> **Deprecated**: Deprecated. Models released after Claude Opus 4.6 do
> not support setting top_p. A value >= 0.99 will be accepted for
> backwards compatibility, all other values will be rejected with a 400
> error.

`claude-opus-5` is released after Claude Opus 4.6, so this is the live
constraint on `iris`. The consequence B2 implements: when
`OutputConfig.Effort` is set, `ToAnthropic` leaves `Temperature` and
`TopP` unset rather than forwarding whatever the client sent. What the
client in fact sends is the separate question recorded in
[`hermes-chat-completions-wire.md`](hermes-chat-completions-wire.md).

## Live run: PENDING — operator ceremony

Cannot run here: no Anthropic key on this machine, and each run is a
billed call. From the Mac, with the operator's key in a file (mode 0600);
the script posts to `https://api.anthropic.com` directly, so no
`ANTHROPIC_BASE_URL` can send it through modelgate by accident:

```sh
bash scripts/probe-anthropic-effort.sh ~/.config/anthropic/probe-key claude-opus-5
```

That is one invocation and four requests — `low`, `xhigh`, `max` and the
bogus `adaptive` — each printing a summary line and a JSON object. Paste
all four pairs verbatim below. The output carries no key material.

### Runs

| # | `output_config.effort` | Expected | HTTP | `stop_reason` | `error.message` |
|---|---|---|---|---|---|
| 1 | `low` | accepted | PENDING | | |
| 2 | `xhigh` | accepted | PENDING | | |
| 3 | `max` | accepted | PENDING | | |
| 4 | `adaptive` | 400 | PENDING | | |

### Verbatim output

```text
PENDING — paste the four summary lines and their JSON objects here.
```

### Verdict

PENDING. Confirm, or correct, three things B2's goldens assume:
`output_config.effort` is the accepted field name; `low`, `xhigh` and
`max` are accepted levels on `claude-opus-5`; a value outside the enum is
a 400 rather than a silent fallback to the default.
