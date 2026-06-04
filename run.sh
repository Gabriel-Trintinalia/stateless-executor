#!/usr/bin/env bash
# run.sh — upload local files as Kurtosis artifacts, then run a command.
#
# Usage:
#   ./run.sh [--enclave NAME] --artifacts <path>[,<path>...] -- <command...>
#
# Each file is uploaded using its basename as the artifact name.
# e.g. ./bin/zesu -> artifact "zesu", ./bin/zesu-zisk -> artifact "zesu-zisk"
#
# Example:
#   ./run.sh \
#     --artifacts ./bin/zesu,./bin/zesu-zisk,./bin/ziskemu \
#     -- kurtosis run --enclave stateless-devnet \
#          github.com/Gabriel-Trintinalia/ethereum-package@feat/stateless-executor \
#          --args-file kurtosis.yaml

set -euo pipefail

ENCLAVE="${ENCLAVE:-stateless-devnet}"
ARTIFACTS=""
CMD=()

# ── Argument parsing ──────────────────────────────────────────────────────────

while [[ $# -gt 0 ]]; do
    case "$1" in
        --enclave)
            ENCLAVE="$2"; shift 2 ;;
        --enclave=*)
            ENCLAVE="${1#*=}"; shift ;;
        --artifacts)
            ARTIFACTS="$2"; shift 2 ;;
        --artifacts=*)
            ARTIFACTS="${1#*=}"; shift ;;
        --)
            shift
            CMD=("$@")
            break ;;
        *)
            echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

if [[ -z "$ARTIFACTS" ]]; then
    echo "error: --artifacts is required" >&2; exit 1
fi
if [[ ${#CMD[@]} -eq 0 ]]; then
    echo "error: no command provided after --" >&2; exit 1
fi

# ── Create enclave (idempotent) ───────────────────────────────────────────────

echo "==> enclave: $ENCLAVE"
kurtosis enclave add --name "$ENCLAVE" 2>/dev/null || true

# ── Upload artifacts ──────────────────────────────────────────────────────────

IFS=',' read -ra PATHS <<< "$ARTIFACTS"
for path in "${PATHS[@]}"; do
    name="$(basename "$path")"
    echo "==> uploading $name: $path"
    kurtosis files upload "$ENCLAVE" "$path" --name "$name"
done

# ── Run command ───────────────────────────────────────────────────────────────

echo "==> ${CMD[*]}"
exec "${CMD[@]}"
