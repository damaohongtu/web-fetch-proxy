#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="${project_dir}/compose.yaml"
env_file="${project_dir}/.env"
pki_dir="${project_dir}/var/pki"
secret_dir="${project_dir}/var/secrets"
log_root="${project_dir}/var/log"

usage() {
  printf '%s\n' \
    "Usage:" \
    "  $0 init <cloud-public-ip> <internal-egress-cidr>" \
    "  $0 preflight" \
    "  $0 start" \
    "  $0 stop" \
    "  $0 restart" \
    "  $0 rebuild" \
    "  $0 status" \
    "  $0 logs [nginx|fetch-worker]" \
    "  $0 verify [public-test-url]" \
    "  $0 export-client <empty-output-directory>"
}

compose() {
  docker compose \
    --project-directory "${project_dir}" \
    --env-file "${env_file}" \
    -f "${compose_file}" "$@"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    printf 'Required command is not installed: %s\n' "$1" >&2
    exit 1
  }
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

is_ipv4_cidr() {
  local value="$1"
  local ip="${value%/*}"
  local prefix="${value#*/}"
  [[ "${value}" == */* ]] || return 1
  is_ipv4 "${ip}" || return 1
  [[ "${prefix}" =~ ^[0-9]+$ ]] && (( prefix >= 0 && prefix <= 32 ))
}

# Each entry is a bare hostname (mcp-server.comein.cn) or host:port.
is_upstream_entry() {
  local value="$1"
  local host port
  if [[ "${value}" == *:* ]]; then
    host="${value%%:*}"
    port="${value#*:}"
  else
    host="${value}"
    port=""
  fi
  [[ "${host}" =~ ^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?$ ]] || return 1
  if [[ -n "${port}" ]]; then
    [[ "${port}" =~ ^[0-9]+$ ]] && (( 10#${port} >= 1 && 10#${port} <= 65535 )) || return 1
  fi
}

load_environment() {
  [[ -s "${env_file}" ]] || {
    printf 'Missing %s. Run: %s init <cloud-public-ip> <internal-egress-cidr>\n' "${env_file}" "$0" >&2
    exit 1
  }
  set -a
  # shellcheck disable=SC1090
  source "${env_file}"
  set +a
}

require_runtime_file() {
  [[ -s "$1" ]] || {
    printf 'Required runtime file is missing: %s\n' "$1" >&2
    exit 1
  }
}

prepare_log_directories() {
  mkdir -p "${log_root}/nginx" "${log_root}/fetch-worker"
  chmod 750 "${log_root}" "${log_root}/nginx" "${log_root}/fetch-worker"
  if [[ "$(uname -s)" == "Linux" && "$(id -u)" == "0" ]]; then
    chown root:root "${log_root}" "${log_root}/nginx"
    # Worker runs with the pinned UID/GID from services/fetch-worker/Dockerfile.
    chown 10001:10001 "${log_root}/fetch-worker"
  fi
}

preflight() {
  require_command docker
  require_command openssl
  require_command cmp
  docker compose version >/dev/null
  docker compose up --help | grep -q -- '--wait' || {
    printf 'Docker Compose is too old; this deployment requires support for compose up --wait.\n' >&2
    exit 1
  }
  docker info >/dev/null 2>&1 || {
    printf 'Docker daemon is not running or is not accessible.\n' >&2
    exit 1
  }
  prepare_log_directories
  load_environment

  is_ipv4 "${SERVER_NAME:-}" || {
    printf 'SERVER_NAME must be the cloud public IPv4 address.\n' >&2
    exit 1
  }
  is_ipv4 "${PUBLIC_BIND_ADDRESS:-127.0.0.1}" || {
    printf 'PUBLIC_BIND_ADDRESS must be a valid IPv4 address.\n' >&2
    exit 1
  }
  if [[ "${SERVER_NAME}" != "127.0.0.1" && "${PUBLIC_BIND_ADDRESS:-127.0.0.1}" != "0.0.0.0" ]]; then
    printf 'Production deployment must publish on PUBLIC_BIND_ADDRESS=0.0.0.0.\n' >&2
    exit 1
  fi
  is_ipv4_cidr "${ALLOWED_CLIENT_CIDR:-}" || {
    printf 'ALLOWED_CLIENT_CIDR must be a valid IPv4 CIDR.\n' >&2
    exit 1
  }
  if [[ "${SERVER_NAME}" != "127.0.0.1" && "${ALLOWED_CLIENT_CIDR}" == "0.0.0.0/0" ]]; then
    printf 'Production deployment must not use ALLOWED_CLIENT_CIDR=0.0.0.0/0.\n' >&2
    exit 1
  fi
  case ",${BLOCKED_CIDRS:-}," in
    *,"${SERVER_NAME}/32",*) ;;
    *)
      printf 'BLOCKED_CIDRS must include the cloud public IP as %s/32.\n' "${SERVER_NAME}" >&2
      exit 1
      ;;
  esac
  if [[ -n "${ALLOWED_UPSTREAM_HOSTS:-}" ]]; then
    while IFS= read -r entry; do
      [[ -n "${entry}" ]] || continue
      is_upstream_entry "${entry}" || {
        printf 'Invalid ALLOWED_UPSTREAM_HOSTS entry %q: expected a hostname such as mcp-server.comein.cn, optionally with :port.\n' "${entry}" >&2
        exit 1
      }
    done < <(tr ',' '\n' <<< "${ALLOWED_UPSTREAM_HOSTS}")
  fi
  if [[ -n "${PUBLIC_PATH_PREFIX:-}" ]]; then
    [[ "${PUBLIC_PATH_PREFIX}" == /* ]] || {
      printf 'PUBLIC_PATH_PREFIX must start with a slash, for example /ia-access-gateway/mcp.\n' >&2
      exit 1
    }
  fi

  require_runtime_file "${pki_dir}/ca.crt"
  require_runtime_file "${pki_dir}/server.crt"
  require_runtime_file "${pki_dir}/server.key"
  require_runtime_file "${pki_dir}/client.crt"
  require_runtime_file "${pki_dir}/client.key"
  require_runtime_file "${secret_dir}/fetch_auth_token"

  openssl verify -CAfile "${pki_dir}/ca.crt" "${pki_dir}/server.crt" "${pki_dir}/client.crt" >/dev/null
  openssl x509 -in "${pki_dir}/server.crt" -noout -checkip "${SERVER_NAME}" >/dev/null
  openssl x509 -checkend 86400 -noout -in "${pki_dir}/server.crt" >/dev/null || {
    printf 'Server certificate is expired or expires within 24 hours.\n' >&2
    exit 1
  }
  openssl x509 -checkend 86400 -noout -in "${pki_dir}/client.crt" >/dev/null || {
    printf 'Client certificate is expired or expires within 24 hours.\n' >&2
    exit 1
  }
  cmp -s \
    <(openssl x509 -in "${pki_dir}/server.crt" -pubkey -noout) \
    <(openssl pkey -in "${pki_dir}/server.key" -pubout) || {
      printf 'Server certificate and private key do not match.\n' >&2
      exit 1
    }
  cmp -s \
    <(openssl x509 -in "${pki_dir}/client.crt" -pubkey -noout) \
    <(openssl pkey -in "${pki_dir}/client.key" -pubout) || {
      printf 'Client certificate and private key do not match.\n' >&2
      exit 1
    }
  if (( $(wc -c < "${secret_dir}/fetch_auth_token") < 64 )); then
    printf 'Bearer token is unexpectedly short. Rotate it before starting.\n' >&2
    exit 1
  fi

  compose config --quiet
  printf 'Preflight passed: configuration, mTLS identities, token, Docker and Compose are ready.\n'
}

init_deployment() {
  local public_ip="${1:-}"
  local internal_cidr="${2:-}"
  local env_new="${env_file}.new"
  local identities_created=0
  is_ipv4 "${public_ip}" || {
    printf 'A valid cloud public IPv4 address is required.\n' >&2
    exit 2
  }
  is_ipv4_cidr "${internal_cidr}" || {
    printf 'A valid internal-server egress CIDR is required, for example 198.51.100.10/32.\n' >&2
    exit 2
  }
  if [[ "${public_ip}" != "127.0.0.1" && "${internal_cidr}" == "0.0.0.0/0" ]]; then
    printf 'Refusing a production deployment open to every source IP.\n' >&2
    exit 2
  fi
  [[ ! -e "${env_file}" ]] || {
    printf '%s already exists; init never overwrites an active deployment.\n' "${env_file}" >&2
    exit 1
  }
  [[ ! -e "${env_new}" ]] || {
    printf 'Stale initialization file exists: %s. Inspect and remove it before retrying.\n' "${env_new}" >&2
    exit 1
  }

  # Nothing below existed before this invocation. If any later step fails, remove only
  # the staging file and identities created by this initialization attempt so init is retryable.
  cleanup_failed_deployment_init() {
    local status=$?
    if (( status != 0 )); then
      rm -f -- "${env_new}"
      if (( identities_created == 1 )); then
        rm -f -- \
          "${pki_dir}/ca.key" "${pki_dir}/ca.crt" \
          "${pki_dir}/server.key" "${pki_dir}/server.crt" \
          "${pki_dir}/client.key" "${pki_dir}/client.crt" \
          "${secret_dir}/fetch_auth_token"
      fi
      printf 'Deployment initialization was rolled back and can be retried safely.\n' >&2
    fi
    exit "${status}"
  }
  trap cleanup_failed_deployment_init EXIT

  umask 077
  {
    printf 'PUBLIC_HTTPS_PORT=443\n'
    printf 'PUBLIC_BIND_ADDRESS=0.0.0.0\n'
    printf 'SERVER_NAME=%s\n' "${public_ip}"
    printf 'ALLOWED_CLIENT_CIDR=%s\n' "${internal_cidr}"
    printf 'BLOCKED_CIDRS=%s/32\n' "${public_ip}"
    printf 'DEFAULT_TIMEOUT_MS=30000\nMAX_TIMEOUT_MS=60000\n'
    printf 'CONNECT_TIMEOUT_MS=5000\nRESPONSE_HEADER_TIMEOUT_MS=15000\n'
    printf 'DEFAULT_MAX_BYTES=10485760\nHARD_MAX_BYTES=52428800\n'
    printf 'DEFAULT_MAX_REDIRECTS=5\nHARD_MAX_REDIRECTS=10\nMAX_CONCURRENCY=100\n'
    printf 'ALLOWED_UPSTREAM_HOSTS=\nPROXY_MAX_CONCURRENCY=50\n'
    printf 'PROXY_MAX_STREAM_BYTES=104857600\nPUBLIC_PATH_PREFIX=\n'
    printf 'WORKER_MEMORY_LIMIT=512m\nWORKER_CPU_LIMIT=1.0\n'
    printf 'NGINX_MEMORY_LIMIT=256m\nNGINX_CPU_LIMIT=0.5\n'
  } > "${env_new}"
  chmod 600 "${env_new}"
  if ! "${project_dir}/scripts/generate-mtls.sh" init "${public_ip}"; then
    printf 'Initialization failed before .env was activated. Resolve the PKI error and retry.\n' >&2
    exit 1
  fi
  identities_created=1
  mv "${env_new}" "${env_file}"
  trap - EXIT
  printf '\nInitialization complete. This did not start containers.\n'
  printf 'Start both cloud components with: %s start\n' "$0"
}

start_services() {
  preflight
  printf '\nStarting cloud services explicitly:\n'
  printf '  1. fetch-worker (Go outbound fetch service)\n'
  printf '  2. nginx (public TCP %s -> container TLS 443, mandatory mTLS)\n\n' "${PUBLIC_HTTPS_PORT}"
  compose up -d --wait --wait-timeout 90 fetch-worker nginx
  compose ps
  printf '\nBoth fetch-worker and nginx are running.\n'
}

export_client() {
  local output="${1:-}"
  [[ -n "${output}" ]] || {
    printf 'An output directory is required.\n' >&2
    exit 2
  }
  load_environment
  require_runtime_file "${pki_dir}/ca.crt"
  require_runtime_file "${pki_dir}/client.crt"
  require_runtime_file "${pki_dir}/client.key"
  require_runtime_file "${secret_dir}/fetch_auth_token"
  if [[ -e "${output}" ]] && [[ -n "$(find "${output}" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]]; then
    printf 'Output directory must not already contain files: %s\n' "${output}" >&2
    exit 1
  fi
  mkdir -p "${output}"
  chmod 700 "${output}"
  install -m 644 "${pki_dir}/ca.crt" "${output}/ca.crt"
  install -m 644 "${pki_dir}/client.crt" "${output}/client.crt"
  install -m 600 "${pki_dir}/client.key" "${output}/client.key"
  install -m 600 "${secret_dir}/fetch_auth_token" "${output}/fetch_auth_token"
  install -m 755 "${project_dir}/examples/internal-web-fetch/internal_web_fetch.py" "${output}/internal_web_fetch.py"
  install -m 755 "${project_dir}/scripts/smoke-test.sh" "${output}/smoke-test.sh"
  endpoint="https://${SERVER_NAME}"
  [[ "${PUBLIC_HTTPS_PORT}" == "443" ]] || endpoint="${endpoint}:${PUBLIC_HTTPS_PORT}"
  printf 'CLOUD_FETCH_ENDPOINT=%s/v1/fetch\n' "${endpoint}" > "${output}/client.env"
  chmod 600 "${output}/client.env"
  printf 'Client bundle created at %s\n' "${output}"
  printf 'It contains client credentials; transfer it through an approved secure channel.\n'
}

command="${1:-}"
case "${command}" in
  init)
    init_deployment "${2:-}" "${3:-}"
    ;;
  preflight)
    preflight
    ;;
  start)
    start_services
    ;;
  stop)
    load_environment
    compose down --remove-orphans
    ;;
  restart)
    preflight
    printf 'Recreating fetch-worker and nginx with the current configuration...\n'
    compose up -d --force-recreate --wait --wait-timeout 90 fetch-worker nginx
    compose ps
    ;;
  rebuild)
    preflight
    printf 'Pulling Nginx and rebuilding fetch-worker, then recreating both services...\n'
    compose pull nginx
    compose build --pull fetch-worker
    compose up -d --force-recreate --wait --wait-timeout 90 fetch-worker nginx
    compose ps
    ;;
  status)
    load_environment
    compose ps
    ;;
  logs)
    load_environment
    service="${2:-}"
    if [[ -n "${service}" && "${service}" != "nginx" && "${service}" != "fetch-worker" ]]; then
      printf 'Service must be nginx or fetch-worker.\n' >&2
      exit 2
    fi
    if [[ -n "${service}" ]]; then
      compose logs --tail=200 -f "${service}"
    else
      compose logs --tail=200 -f nginx fetch-worker
    fi
    ;;
  verify)
    load_environment
    "${project_dir}/scripts/smoke-test.sh" "${2:-https://www.baidu.com/}"
    ;;
  export-client)
    export_client "${2:-}"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
