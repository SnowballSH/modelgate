#!/usr/bin/env bash
set -Eeuo pipefail
umask 0077

key_file="${1:?usage: probe-gpt56-tools.sh <key-file> [model] [effort]}"
model="${2:-gpt-5.6-luna}"
effort="${3:-medium}"

command -v jq >/dev/null || {
  printf 'jq is required\n' >&2
  exit 1
}
[ -r "${key_file}" ] || {
  printf 'cannot read key file %s\n' "${key_file}" >&2
  exit 1
}

out="$(mktemp)"
hdr="$(mktemp)"
trap 'rm -f "${out}" "${hdr}"' EXIT
chmod 0600 "${hdr}"
printf 'Authorization: Bearer %s\n' "$(<"${key_file}")" > "${hdr}"

body=$(cat <<JSON
{"model":"${model}","reasoning_effort":"${effort}","messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}],
 "tools":[{"type":"function","function":{"name":"get_weather","description":"Weather by city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
 "tool_choice":"auto"}
JSON
)

status="$(curl -sS -o "${out}" -w '%{http_code}' https://api.openai.com/v1/chat/completions \
  -H @"${hdr}" -H 'Content-Type: application/json' --data "${body}")"
printf 'model=%s reasoning_effort=%s tools=1 -> HTTP %s\n' "${model}" "${effort}" "${status}"
jq '{error: .error, finish_reason: .choices[0].finish_reason, tool_calls: .choices[0].message.tool_calls, usage: .usage}' "${out}"
