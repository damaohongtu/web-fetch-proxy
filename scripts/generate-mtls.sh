#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pki_dir="${project_dir}/var/pki"
secret_dir="${project_dir}/var/secrets"

mkdir -p "${pki_dir}" "${secret_dir}"
umask 077

usage() {
  printf '%s\n' \
    "Usage:" \
    "  $0 init <cloud-public-ip>       Create CA, server/client certificates and token" \
    "  $0 server <cloud-public-ip>     Rotate only the server certificate" \
    "  $0 client [client-name]         Rotate only the client certificate" \
    "  $0 token                        Rotate only the Bearer token" \
    "  $0 inspect                      Show certificate identity and validity"
}

is_ipv4() {
  local value="$1"
  local part
  local -a parts
  [[ "${value}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  IFS='.' read -r -a parts <<< "${value}"
  [[ "${#parts[@]}" -eq 4 ]] || return 1
  for part in "${parts[@]}"; do
    (( 10#${part} <= 255 )) || return 1
  done
}

require_file() {
  [[ -s "$1" ]] || {
    printf 'Required file is missing: %s\n' "$1" >&2
    exit 1
  }
}

ensure_new_install() {
  local path
  for path in \
    "${pki_dir}/ca.key" "${pki_dir}/ca.crt" \
    "${pki_dir}/server.key" "${pki_dir}/server.crt" \
    "${pki_dir}/client.key" "${pki_dir}/client.crt" \
    "${secret_dir}/fetch_auth_token"; do
    if [[ -e "${path}" ]]; then
      printf 'Refusing to overwrite existing runtime identity: %s\n' "${path}" >&2
      printf 'Use the server, client, or token command for an intentional rotation.\n' >&2
      exit 1
    fi
  done
}

create_ca() {
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "${pki_dir}/ca.key.new"
  openssl req -x509 -new -sha256 -days 3650 \
    -key "${pki_dir}/ca.key.new" \
    -config <(printf '%s\n' \
      '[req]' \
      'distinguished_name=req_dn' \
      'x509_extensions=v3_ca' \
      'prompt=no' \
      '[req_dn]' \
      'CN=Web Fetch Proxy Private CA' \
      '[v3_ca]' \
      'basicConstraints=critical,CA:TRUE,pathlen:0' \
      'keyUsage=critical,keyCertSign,cRLSign' \
      'subjectKeyIdentifier=hash' \
      'authorityKeyIdentifier=keyid:always') \
    -extensions v3_ca \
    -out "${pki_dir}/ca.crt.new"
  mv "${pki_dir}/ca.key.new" "${pki_dir}/ca.key"
  mv "${pki_dir}/ca.crt.new" "${pki_dir}/ca.crt"
}

issue_server() {
  local public_ip="$1"
  local key_new="${pki_dir}/server.key.new"
  local csr_new="${pki_dir}/server.csr.new"
  local crt_new="${pki_dir}/server.crt.new"

  is_ipv4 "${public_ip}" || {
    printf 'A valid IPv4 address is required, got: %s\n' "${public_ip}" >&2
    exit 1
  }
  require_file "${pki_dir}/ca.key"
  require_file "${pki_dir}/ca.crt"

  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "${key_new}"
  openssl req -new -sha256 -key "${key_new}" -subj "/CN=${public_ip}" -out "${csr_new}"
  openssl x509 -req -sha256 -days 397 \
    -in "${csr_new}" \
    -CA "${pki_dir}/ca.crt" \
    -CAkey "${pki_dir}/ca.key" \
    -CAcreateserial \
    -extfile <(printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=IP:%s\n' "${public_ip}") \
    -out "${crt_new}"
  openssl verify -CAfile "${pki_dir}/ca.crt" "${crt_new}"
  openssl x509 -in "${crt_new}" -noout -checkip "${public_ip}" >/dev/null
  mv "${key_new}" "${pki_dir}/server.key"
  mv "${crt_new}" "${pki_dir}/server.crt"
  rm -f "${csr_new}" "${pki_dir}/ca.srl"
}

issue_client() {
  local client_name="${1:-internal-web-fetch}"
  local key_new="${pki_dir}/client.key.new"
  local csr_new="${pki_dir}/client.csr.new"
  local crt_new="${pki_dir}/client.crt.new"

  [[ "${client_name}" =~ ^[A-Za-z0-9._-]+$ ]] || {
    printf 'Client name may only contain letters, digits, dot, underscore and hyphen.\n' >&2
    exit 1
  }
  require_file "${pki_dir}/ca.key"
  require_file "${pki_dir}/ca.crt"

  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "${key_new}"
  openssl req -new -sha256 -key "${key_new}" -subj "/CN=${client_name}" -out "${csr_new}"
  openssl x509 -req -sha256 -days 397 \
    -in "${csr_new}" \
    -CA "${pki_dir}/ca.crt" \
    -CAkey "${pki_dir}/ca.key" \
    -CAcreateserial \
    -extfile <(printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth\n') \
    -out "${crt_new}"
  openssl verify -CAfile "${pki_dir}/ca.crt" "${crt_new}"
  mv "${key_new}" "${pki_dir}/client.key"
  mv "${crt_new}" "${pki_dir}/client.crt"
  rm -f "${csr_new}" "${pki_dir}/ca.srl"
}

rotate_token() {
  openssl rand -hex 32 > "${secret_dir}/fetch_auth_token.new"
  mv "${secret_dir}/fetch_auth_token.new" "${secret_dir}/fetch_auth_token"
}

secure_permissions() {
  find "${pki_dir}" -type f -name '*.key' -exec chmod 600 {} +
  find "${pki_dir}" -type f -name '*.crt' -exec chmod 644 {} +
  if [[ -e "${secret_dir}/fetch_auth_token" ]]; then
    if [[ "$(uname -s)" == "Linux" && "$(id -u)" == "0" ]]; then
      # Compose file secrets are bind mounts on Linux; match the pinned container UID.
      chown 10001:10001 "${secret_dir}/fetch_auth_token"
      chmod 400 "${secret_dir}/fetch_auth_token"
    else
      chmod 600 "${secret_dir}/fetch_auth_token"
    fi
  fi
}

command="${1:-}"
case "${command}" in
  init)
    public_ip="${2:-}"
    is_ipv4 "${public_ip}" || {
      printf 'A valid cloud public IPv4 address is required.\n' >&2
      usage >&2
      exit 2
    }
    ensure_new_install
    cleanup_failed_init() {
      local status=$?
      if (( status != 0 )); then
        rm -f -- \
          "${pki_dir}/ca.key" "${pki_dir}/ca.crt" \
          "${pki_dir}/server.key" "${pki_dir}/server.crt" \
          "${pki_dir}/client.key" "${pki_dir}/client.crt" \
          "${pki_dir}"/*.new "${pki_dir}/ca.srl" \
          "${secret_dir}/fetch_auth_token" "${secret_dir}/fetch_auth_token.new"
        printf 'PKI initialization failed; partial files were removed.\n' >&2
      fi
      exit "${status}"
    }
    trap cleanup_failed_init EXIT
    create_ca
    issue_server "${public_ip}"
    issue_client "internal-web-fetch"
    rotate_token
    secure_permissions
    trap - EXIT
    printf 'Created mTLS identities for cloud IP %s.\n' "${public_ip}"
    printf 'Distribute only ca.crt, client.crt, client.key and the Bearer token to the internal server.\n'
    ;;
  server)
    issue_server "${2:-}"
    secure_permissions
    printf 'Rotated the server certificate. Restart Nginx to load it.\n'
    ;;
  client)
    issue_client "${2:-internal-web-fetch}"
    secure_permissions
    printf 'Rotated the client certificate files. This does not revoke previously issued certificates; see docs/operations.md.\n'
    ;;
  token)
    rotate_token
    secure_permissions
    printf 'Rotated the Bearer token. Recreate the Worker and update the internal server together.\n'
    ;;
  inspect)
    require_file "${pki_dir}/server.crt"
    require_file "${pki_dir}/client.crt"
    printf '%s\n' 'Server certificate:'
    openssl x509 -in "${pki_dir}/server.crt" -noout -subject -issuer -dates
    openssl x509 -in "${pki_dir}/server.crt" -noout -ext subjectAltName
    openssl x509 -in "${pki_dir}/server.crt" -noout -ext extendedKeyUsage
    printf '%s\n' 'Client certificate:'
    openssl x509 -in "${pki_dir}/client.crt" -noout -subject -issuer -dates
    openssl x509 -in "${pki_dir}/client.crt" -noout -ext extendedKeyUsage
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
