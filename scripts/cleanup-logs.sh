#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="${project_dir}/compose.yaml"
env_file="${project_dir}/.env"
log_root="${project_dir}/var/log"
retention_days="${1:-7}"

usage() {
  printf 'Usage: %s [retention-days]\n' "$0"
}

if [[ ! "${retention_days}" =~ ^[0-9]+$ ]] || (( retention_days < 1 || retention_days > 3650 )); then
  usage >&2
  printf 'retention-days must be an integer between 1 and 3650.\n' >&2
  exit 2
fi

for command_name in docker gzip find date; do
  command -v "${command_name}" >/dev/null 2>&1 || {
    printf 'Required command is not installed: %s\n' "${command_name}" >&2
    exit 1
  }
done

[[ -s "${compose_file}" && -s "${env_file}" ]] || {
  printf 'Missing compose.yaml or .env under %s.\n' "${project_dir}" >&2
  exit 1
}

mkdir -p "${log_root}/nginx" "${log_root}/fetch-worker"
lock_dir="${log_root}/.cleanup.lock"
if ! mkdir "${lock_dir}" 2>/dev/null; then
  printf 'Another log cleanup process is already running: %s\n' "${lock_dir}" >&2
  exit 1
fi
cleanup_lock() {
  rmdir "${lock_dir}" 2>/dev/null || true
}
trap cleanup_lock EXIT

compose() {
  docker compose \
    --project-directory "${project_dir}" \
    --env-file "${env_file}" \
    -f "${compose_file}" "$@"
}

timestamp="$(date -u '+%Y%m%dT%H%M%SZ')"
nginx_rotated=0
worker_rotated=0
declare -a archives=()

for active_log in "${log_root}/nginx/access.log" "${log_root}/nginx/error.log"; do
  if [[ -s "${active_log}" ]]; then
    archive="${active_log}.${timestamp}"
    mv "${active_log}" "${archive}"
    archives+=("${archive}")
    nginx_rotated=$((nginx_rotated + 1))
  fi
done

worker_log="${log_root}/fetch-worker/worker.log"
if [[ -s "${worker_log}" ]]; then
  worker_archive="${worker_log}.${timestamp}"
  mv "${worker_log}" "${worker_archive}"
  archives+=("${worker_archive}")
  worker_rotated=1
fi

if (( ${#archives[@]} > 0 )); then
  # A deliberate short interruption guarantees every process closes the archived inode
  # and opens a new active log before compression.
  compose up -d --force-recreate --wait --wait-timeout 90 fetch-worker nginx
  for archive in "${archives[@]}"; do
    gzip -f "${archive}"
  done
fi

retention_minutes=$((retention_days * 24 * 60))
deleted_count=0
while IFS= read -r -d '' expired_log; do
  printf 'Deleting expired log archive: %s\n' "${expired_log}"
  rm -f -- "${expired_log}"
  deleted_count=$((deleted_count + 1))
done < <(find "${log_root}" -type f -name '*.log.*' -mmin "+${retention_minutes}" -print0)

printf 'Log maintenance complete: retention=%s days, nginx_rotated=%s, worker_rotated=%s, deleted=%s.\n' \
  "${retention_days}" "${nginx_rotated}" "${worker_rotated}" "${deleted_count}"
