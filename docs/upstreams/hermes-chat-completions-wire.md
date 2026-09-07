# What Hermes actually puts on the wire

Recorded 2026-09-07.
**Live capture: PENDING — operator ceremony** (see below).

Every other record under `docs/upstreams/` describes modelgate talking to
a provider. This one describes the other side: the Chat Completions body
Hermes sends to modelgate. Nothing else in the plan records it, and two
translators depend on it.

## Why it matters

Anthropic refuses the sampling parameters for the models the farm runs.
The Messages API reference is quoted in full in
[`anthropic-output-config-effort-probe.md`](anthropic-output-config-effort-probe.md);
in short, on models released after Claude Opus 4.6 a `temperature` other
than `1.0` and a `top_p` below `0.99` are each a 400. Today
`translate.ToAnthropic` copies `Temperature` and `TopP` from the request
with no guard, so a non-default value from Hermes would 400 every `iris`
turn upstream. On the OpenAI side the same value would be refused
locally, by `translate.ToResponses`.

So the failure this capture prevents is a worker fleet that 400s on its
first real turn, at F2/V4, after the image is pinned — and which nothing
before that point would have noticed.

## Documented contract

None. Hermes publishes no wire contract for its OpenAI-compatible
transport, and its defaults are a property of the pinned commit rather
than of a specification. The only sound way to know what it sends is to
execute its transport and read the body — which is how SPEC §1 settled
`reasoning_effort`.

## The resolution this capture confirms

The translators do not wait on the capture. Both are built to accept
whatever Hermes sends:

- `translate.ToAnthropic` (B2) drops `Temperature` and `TopP` — leaves
  them unset — whenever `OutputConfig.Effort` is set, rather than
  forwarding a value Anthropic will reject.
- `translate.ToResponses` (B5) normalises Hermes' fixed defaults instead
  of rejecting them.

The capture is therefore a confirmation step before the `v0.5.0` tag, not
a gate on B4/B5. It becomes load-bearing only if it shows a value neither
translator anticipated — a `temperature` Hermes will not let the operator
change, say — in which case the normalisation is what has to move.

## Live capture: PENDING — operator ceremony

Cannot run here: this machine has no Hermes install, and the capture
needs one real agent turn from a worker profile. On a host that has one:

```sh
bash scripts/capture-hermes-wire.sh 8099 /tmp/hermes-wire.jsonl
```

It prints the base URL to point Hermes at and then listens on loopback.
Point a worker profile at `http://127.0.0.1:8099/v1` with any key value,
run **one** turn that carries tools and a reasoning level, and stop the
listener with ctrl-c.

The listener answers both streaming and non-streaming requests with a
minimal valid completion, so the turn finishes normally. It records
request bodies only — never headers — so the key Hermes sends is not
written anywhere.

### Fields to paste back verbatim

The summary line the script prints for the turn, which names every field
that decides this:

```text
PENDING — paste the summary line here, e.g.
model="…" stream=… temperature=… top_p=… reasoning_effort="…" tools=N messages=N
```

and the captured body, pretty-printed
(`jq . /tmp/hermes-wire.jsonl`), with any prompt text the operator does
not wish to publish elided:

```json
PENDING — paste the captured request body here.
```

### Verdict

PENDING. Answer three questions:

1. Does `temperature` appear, and with what value?
2. Does `top_p` appear, and with what value?
3. Is either value configurable in the Hermes profile, or fixed by the
   transport?

If a value appears that neither translator normalises, say so here and
open the correction against B2 and B5 before the tag.
