#!/usr/bin/env bash
set -Eeuo pipefail
umask 0077

key_file="${1:?usage: probe-gpt56-responses.sh <key-file> [model] [effort]}"
model="${2:-gpt-5.6-luna}"
effort="${3:-xhigh}"

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
{"model":"${model}","store":false,"reasoning":{"effort":"${effort}"},
 "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the weather in Paris? Use the tool."}]},
          {"type":"function_call","call_id":"call_probe_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},
          {"type":"function_call_output","call_id":"call_probe_1","output":"18C and clear"},
          {"type":"message","role":"user","content":[{"type":"input_text","text":"And tomorrow? Use the tool again."}]}],
 "tools":[{"type":"function","name":"get_weather","description":"Weather by city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
 "tool_choice":"auto"}
JSON
)

status="$(curl -sS -o "${out}" -w '%{http_code}' https://api.openai.com/v1/responses \
  -H @"${hdr}" -H 'Content-Type: application/json' --data "${body}")"
printf 'model=%s reasoning.effort=%s store=false call_id-only reconstruction -> HTTP %s\n' "${model}" "${effort}" "${status}"
jq '{error: .error, status: .status, incomplete_details: .incomplete_details,
     output: [.output[]? | {type, name, call_id, arguments,
                            text: ([.content[]? | select(.type == "output_text") | .text] | join(""))}],
     usage: .usage}' "${out}"
