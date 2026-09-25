# GPT-5.6 on Chat Completions: tools with a non-`none` `reasoning_effort`

Recorded 2026-09-07 from vendor documentation.
**Live run: not performed; the question is moot for the shipped routing** (see Status).

## Status (2026-09-25)

The probe below was never run, and the M1/M2b gateway checks of
2026-09-11 do not answer it: M2b sent `gpt-5.6-luna` through the
Responses API (`upstream_api: responses`), not Chat Completions. What the
Chat Completions upstream does with tools plus a non-`none` effort on
GPT-5.6 therefore remains documented-only. It no longer decides anything
shipped: a model that needs tools with reasoning is routed through the
Responses path, which M2b confirmed live (see
[`openai-gpt56-responses-probe.md`](openai-gpt56-responses-probe.md)).
The credential-rejection body shapes at the end of this record are also
still documented-only.

This is the premise the Responses path is built on. If GPT-5.6 accepts a
Chat Completions request that carries both a function tool and a
`reasoning_effort` other than `none`, modelgate could keep passing worker
traffic straight through; if it does not, every farm worker turn — tools
plus `xhigh` — needs the Responses reconstruction instead.

## Documented contract

> Starting with GPT-5.4, Chat Completions does not support tool calling
> with `reasoning_effort` values other than `none`.

— OpenAI, *Migrate to the Responses API*,
<https://developers.openai.com/api/docs/guides/migrate-to-responses>
(read 2026-09-07).

`gpt-5.6-luna`, `gpt-5.6-terra` and `gpt-5.6-sol` are all later than
GPT-5.4, so on the documented contract the combination is unsupported at
every effort level the farm uses: workers run tools at `xhigh` and the
auxiliary slots run tools at `low`. Only the control run
(`reasoning_effort: "none"`) is inside the supported set.

The reference states that the combination is not supported; it does not
say what the API *does* with it — a 400, or a 200 whose answer simply
never calls the tool. That is the one thing the live run adds, and it is
the difference between a loud failure and a farm that silently stops
using its tools.

## What the outcome decides

| Live result | Consequence for B5–B8 |
|---|---|
| 400 | Required for correctness — the worker fleet cannot run on Chat Completions at all. |
| 200 with no `tool_calls` | Required for correctness — worse than a 400, because it fails silently. |
| 200 with `tool_calls` | Required for parity with the documented contract — the constraint is documented, so a later enforcement would break the farm without warning. |

The tasks are built either way. The probe does not gate them; it records
which of those three sentences the record gets to make.

That is the controller's Stream B resolution, and it governs every record
in this directory: B2 and B4–B8 are written against the documented
contract each one states, and the live runs are a confirmation step before
the `v0.5.0` tag rather than a gate on the code — despite the brief's own
wording, which gates B4 on the Responses probe and B2 on the Anthropic
one. A run that contradicts its documented contract is corrected in the
translator before the tag.

## Direct probe (never run; see Status) — operator ceremony

Cannot run here: this machine holds no OpenAI key, and each run is a
billed call. Run it from the Mac with the supervisor's probe key in a
file (mode 0600 — the script reads the file, never argv or the
environment):

```sh
bash scripts/probe-gpt56-tools.sh ~/.config/openai/probe-key gpt-5.6-luna
bash scripts/probe-gpt56-tools.sh ~/.config/openai/probe-key gpt-5.6-terra
bash scripts/probe-gpt56-tools.sh ~/.config/openai/probe-key gpt-5.6-sol
bash scripts/probe-gpt56-tools.sh ~/.config/openai/probe-key gpt-5.6-luna none
```

Each run prints one summary line and one JSON object. Paste all four
pairs verbatim below, fill the table from them, and record the date. The
output carries no key material.

### Runs

| # | Model | `reasoning_effort` | HTTP | `finish_reason` | `tool_calls` | `error.message` |
|---|---|---|---|---|---|---|
| 1 | `gpt-5.6-luna` | `medium` | PENDING | | | |
| 2 | `gpt-5.6-terra` | `medium` | PENDING | | | |
| 3 | `gpt-5.6-sol` | `medium` | PENDING | | | |
| 4 | `gpt-5.6-luna` | `none` (control) | PENDING | | | |

### Verbatim output

```text
PENDING — paste the four summary lines and their JSON objects here.
```

### Verdict

PENDING — one of the three rows of the table above, naming the run that
shows it.

## Credential-rejection body shapes: documented, not probed

`provider.logsUpstreamMessage` suppresses the upstream `error.message` log
for 401 and 403 because OpenAI's rejection bodies quote the key they
refused, masked only in the middle:
`{"error":{"message":"Incorrect API key provided: sk-…"}}`. That shape is
what B7 read from OpenAI's error-code documentation; no live 401 or 403 was
observed here, so the gate rests on the documentation alone.

A wrong guess about the shape costs a diagnostic line, not a leak:
`redactCredentials` runs over every message that is logged, so a
key-shaped string in a body this record has not seen is replaced before it
reaches the log.

To close this: run any probe script above against a key file holding a
deliberately wrong key and paste the status and body here, with the key
material elided. The call is refused before it is billed.

```text
PENDING — paste the 401 status line and body here (key material elided).
```
