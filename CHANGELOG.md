# Changelog

## v0.5.0 — 2026-09-07

- `reasoning_effort` on Anthropic models is translated to `output_config.effort`
  (`none`/`minimal` → `low`; unknown levels are a 400) instead of rejected.
- `developer` messages fold into the Anthropic `system` prompt.
- Model table: `upstream_api: responses` (OpenAI models only) routes a model
  through the Responses API; Chat Completions stays the default. OpenAI
  documents GPT-5.6 function tools on Chat Completions as compatible only
  with reasoning `none`.
- `usage.completion_tokens_details.reasoning_tokens` is reported when the
  upstream provides it; reasoning tokens are booked as output tokens.
- Known limitation of the Responses upstream: Chat Completions carries no
  reasoning items, so modelgate never echoes them and the model re-reasons
  after every tool result; `max_tokens` caps reasoning and output together.
- `--version` prints the release version.
