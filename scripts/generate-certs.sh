#!/usr/bin/env bash
set -euo pipefail

server_name="${1:-localhost}"
project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cert_dir="${project_dir}/certs"
secret_dir="${project_dir}/secrets"

mkdir -p "${cert_dir}" "${secret_dir}"
umask 077

required_files=(
  "${cert_dir}/ca.key"
  "${cert_dir}/ca.crt"
  "${cert_dir}/server.key"
  "${cert_dir}/server.crt"
  "${cert_dir}/client.key"
  "${cert_dir}/client.crt"
)

if [[ "${FORCE:-0}" != "1" ]]; then
  for path in "${required_files[@]}"; do
    if [[ -e "${path}" ]]; then
      echo "Refusing to overwrite existing certificate file: ${path}" >&2
      echo "Set FORCE=1 only when intentional certificate rotation is required." >&2
      exit 1
    fi
  done
fi

rm_if_forced() {
  if [[ "${FORCE:-0}" == "1" ]]; then
    rm -f -- "$@"
  fi
}

rm_if_forced "${required_files[@]}" "${cert_dir}/server.csr" "${cert_dir}/client.csr" "${cert_dir}/ca.srl"

openssl genpkey -quiet -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "${cert_dir}/ca.key"
openssl req -x509 -new -sha256 -days 3650 \
  -key "${cert_dir}/ca.key" \
  -subj "/CN=Web Fetch Proxy Local CA" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -out "${cert_dir}/ca.crt"

openssl genpkey -quiet -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "${cert_dir}/server.key"
openssl req -new -sha256 \
  -key "${cert_dir}/server.key" \
  -subj "/CN=${server_name}" \
  -out "${cert_dir}/server.csr"

san="DNS:${server_name},DNS:localhost,IP:127.0.0.1"
if [[ "${server_name}" =~ ^[0-9a-fA-F:.]+$ ]]; then
  san="IP:${server_name},DNS:localhost,IP:127.0.0.1"
fi

openssl x509 -req -sha256 -days 397 \
  -in "${cert_dir}/server.csr" \
  -CA "${cert_dir}/ca.crt" \
  -CAkey "${cert_dir}/ca.key" \
  -CAcreateserial \
  -extfile <(printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=%s\n' "${san}") \
  -out "${cert_dir}/server.crt"

openssl genpkey -quiet -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "${cert_dir}/client.key"
openssl req -new -sha256 \
  -key "${cert_dir}/client.key" \
  -subj "/CN=internal-web-fetch" \
  -out "${cert_dir}/client.csr"

openssl x509 -req -sha256 -days 397 \
  -in "${cert_dir}/client.csr" \
  -CA "${cert_dir}/ca.crt" \
  -CAkey "${cert_dir}/ca.key" \
  -CAcreateserial \
  -extfile <(printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth\n') \
  -out "${cert_dir}/client.crt"

rm -f "${cert_dir}/server.csr" "${cert_dir}/client.csr" "${cert_dir}/ca.srl"

if [[ ! -s "${secret_dir}/fetch_auth_token" || "${FORCE:-0}" == "1" ]]; then
  openssl rand -hex 32 > "${secret_dir}/fetch_auth_token"
fi

chmod 600 "${cert_dir}"/*.key "${secret_dir}/fetch_auth_token"
chmod 644 "${cert_dir}"/*.crt

openssl verify -CAfile "${cert_dir}/ca.crt" "${cert_dir}/server.crt" "${cert_dir}/client.crt"
echo "Generated mTLS certificates for ${server_name}."
echo "Keep ca.key and server.key on the cloud server; copy only ca.crt, client.crt and client.key to the internal server."
