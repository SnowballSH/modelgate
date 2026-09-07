#!/usr/bin/env bash
set -Eeuo pipefail
umask 0077

key_file="${1:?usage: probe-anthropic-effort.sh <key-file> [model] [effort ...]}"
model="${2:-claude-opus-5}"
shift $(( $# < 2 ? $# : 2 ))
efforts=("$@")
if [ ${#efforts[@]} -eq 0 ]; then
  efforts=(low xhigh max adaptive)
fi

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
{
  printf 'x-api-key: %s\n' "$(<"${key_file}")"
  printf 'anthropic-version: 2023-06-01\n'
} > "${hdr}"

for effort in "${efforts[@]}"; do
  body=$(cat <<JSON
{"model":"${model}","max_tokens":256,"output_config":{"effort":"${effort}"},
 "messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}],
 "tools":[{"name":"get_weather","description":"Weather by city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}
JSON
)
  status="$(curl -sS -o "${out}" -w '%{http_code}' https://api.anthropic.com/v1/messages \
    -H @"${hdr}" -H 'Content-Type: application/json' --data "${body}")"
  printf 'model=%s output_config.effort=%s tools=1 -> HTTP %s\n' "${model}" "${effort}" "${status}"
  jq '{error: .error, stop_reason: .stop_reason,
       content: [.content[]? | {type, text, name, input}],
       usage: .usage}' "${out}"
done
