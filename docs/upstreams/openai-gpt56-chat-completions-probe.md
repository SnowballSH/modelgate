# GPT-5.6 on Chat Completions: tools with a non-`none` `reasoning_effort`

Recorded 2026-09-07 from vendor documentation.
**Live run: PENDING — operator ceremony** (see below).

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

## Live run: PENDING — operator ceremony

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
