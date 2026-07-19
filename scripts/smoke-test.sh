#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project_dir="$(cd "${script_dir}/.." && pwd)"
public_test_url="${1:-https://www.baidu.com/}"

if [[ -s "${script_dir}/client.env" && -s "${script_dir}/ca.crt" ]]; then
  # Exported client bundle mode: run this on the internal server.
  set -a
  # shellcheck disable=SC1091
  source "${script_dir}/client.env"
  set +a
  endpoint="${CLOUD_FETCH_ENDPOINT%/v1/fetch}"
  pki_dir="${script_dir}"
  token_file="${script_dir}/fetch_auth_token"
else
  # Repository mode: useful for local testing or clouds that permit public-IP hairpin.
  env_file="${project_dir}/.env"
  [[ -s "${env_file}" ]] || {
    printf 'Missing repository .env or adjacent client.env.\n' >&2
    exit 1
  }
  set -a
  # shellcheck disable=SC1090
  source "${env_file}"
  set +a
  endpoint="https://${SERVER_NAME}"
  [[ "${PUBLIC_HTTPS_PORT}" == "443" ]] || endpoint="${endpoint}:${PUBLIC_HTTPS_PORT}"
  pki_dir="${project_dir}/var/pki"
  token_file="${project_dir}/var/secrets/fetch_auth_token"
fi

mtls=(--cacert "${pki_dir}/ca.crt" --cert "${pki_dir}/client.crt" --key "${pki_dir}/client.key")
tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "${tmp_dir}"' EXIT

printf '[1/5] Rejecting a request without a client certificate... '
status="$(curl -sS -o "${tmp_dir}/no-cert.out" -w '%{http_code}' --cacert "${pki_dir}/ca.crt" "${endpoint}/healthz" || true)"
[[ "${status}" != "200" ]] || {
  printf 'FAILED (HTTP %s)\n' "${status}" >&2
  exit 1
}
printf 'OK (HTTP %s)\n' "${status}"

printf '[2/5] Reaching Nginx and Worker through mTLS... '
status="$(curl -sS -o "${tmp_dir}/health.out" -w '%{http_code}' "${mtls[@]}" "${endpoint}/healthz")"
[[ "${status}" == "200" ]] && grep -q '"status":"ok"' "${tmp_dir}/health.out" || {
  printf 'FAILED (HTTP %s)\n' "${status}" >&2
  exit 1
}
printf 'OK\n'

printf '[3/5] Rejecting an invalid Bearer token... '
status="$(curl -sS -o "${tmp_dir}/bad-token.out" -w '%{http_code}' "${mtls[@]}" \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer intentionally-invalid' \
  -d '{"url":"https://1.1.1.1/cdn-cgi/trace"}' "${endpoint}/v1/fetch")"
[[ "${status}" == "401" ]] && grep -q '"code":"UNAUTHORIZED"' "${tmp_dir}/bad-token.out" || {
  printf 'FAILED (HTTP %s)\n' "${status}" >&2
  exit 1
}
printf 'OK\n'

token="$(tr -d '\r\n' < "${token_file}")"
printf '[4/5] Blocking an SSRF request to loopback... '
status="$(curl -sS -o "${tmp_dir}/ssrf.out" -w '%{http_code}' "${mtls[@]}" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer ${token}" \
  -d '{"url":"http://127.0.0.1"}' "${endpoint}/v1/fetch")"
[[ "${status}" == "403" ]] && grep -q '"code":"DESTINATION_BLOCKED"' "${tmp_dir}/ssrf.out" || {
  printf 'FAILED (HTTP %s)\n' "${status}" >&2
  exit 1
}
printf 'OK\n'

printf '[5/5] Fetching a public HTTPS target... '
payload="$(printf '{"url":"%s","max_bytes":1048576}' "${public_test_url}")"
status="$(curl -sS -o "${tmp_dir}/fetch.out" -w '%{http_code}' "${mtls[@]}" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer ${token}" \
  -d "${payload}" "${endpoint}/v1/fetch")"
[[ "${status}" == "200" ]] && grep -q '"status":200' "${tmp_dir}/fetch.out" || {
  printf 'FAILED (HTTP %s)\n' "${status}" >&2
  sed -n '1,20p' "${tmp_dir}/fetch.out" >&2
  exit 1
}
printf 'OK\n\nAll mTLS, authentication, SSRF and public-fetch checks passed.\n'
