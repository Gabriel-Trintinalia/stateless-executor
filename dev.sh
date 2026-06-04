#!/usr/bin/env bash
# dev.sh — start a Besu+Teku devnet and run the stateless executor against it.
#
# Usage:
#   ./dev.sh --zesu <path> [--block <number>] [--enclave <name>]
#
# Examples:
#   ./dev.sh --zesu ./path/to/zesu
#   ./dev.sh --zesu ./path/to/zesu --block 42
#   ./dev.sh --zesu ./path/to/zesu --enclave my-devnet

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENCLAVE="stateless-devnet"
ZESU=""
BLOCK=""

# ── Argument parsing ──────────────────────────────────────────────────────────

while [ "$#" -gt 0 ]; do
    case "$1" in
        --zesu)     ZESU="$2";         shift; shift ;;
        --zesu=*)   ZESU="${1#*=}";    shift ;;
        --block)    BLOCK="$2";        shift; shift ;;
        --block=*)  BLOCK="${1#*=}";   shift ;;
        --enclave)  ENCLAVE="$2";      shift; shift ;;
        --enclave=*) ENCLAVE="${1#*=}"; shift ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

if [ -z "$ZESU" ]; then
    echo "error: --zesu <path> is required" >&2
    echo "usage: $0 --zesu <path> [--block <number>] [--enclave <name>]" >&2
    exit 1
fi

# ── Start enclave ─────────────────────────────────────────────────────────────

echo "==> starting enclave: $ENCLAVE"
kurtosis enclave rm --force "$ENCLAVE" 2>/dev/null || true
kurtosis run --enclave "$ENCLAVE" \
    github.com/ethpandaops/ethereum-package \
    --args-file "$SCRIPT_DIR/devnet.yaml"

# ── Get Besu RPC URL ──────────────────────────────────────────────────────────

echo "==> fetching Besu RPC URL"
EL_SERVICE=$(kurtosis enclave inspect "$ENCLAVE" 2>/dev/null \
    | awk '/User Services/{found=1} found && /el-1-besu/{print $2; exit}')
if [ -z "$EL_SERVICE" ]; then
    echo "error: could not find el-1-besu-* service in enclave" >&2; exit 1
fi
EL_RPC_URL="http://$(kurtosis port print "$ENCLAVE" "$EL_SERVICE" rpc)"
echo "    $EL_SERVICE → $EL_RPC_URL"

echo "==> fetching engine RPC URL"
ENGINE_SERVICE=$(kurtosis enclave inspect "$ENCLAVE" 2>/dev/null \
    | awk '/User Services/{found=1} found && /snooper-engine/{print $2; exit}')
if [ -n "$ENGINE_SERVICE" ]; then
    ENGINE_RPC_URL="http://$(kurtosis port print "$ENCLAVE" "$ENGINE_SERVICE" engine-rpc)"
    echo "    $ENGINE_SERVICE → $ENGINE_RPC_URL"
fi

# ── Download genesis and JWT ──────────────────────────────────────────────────

GENESIS_DIR=$(mktemp -d)
echo "==> downloading genesis to $GENESIS_DIR"
kurtosis files download "$ENCLAVE" el_cl_genesis_data "$GENESIS_DIR"

JWT_DIR=$(mktemp -d)
echo "==> downloading JWT secret"
kurtosis files download "$ENCLAVE" jwt_file "$JWT_DIR"

# ── Run stateless executor ────────────────────────────────────────────────────

echo "==> starting stateless executor (zesu: $ZESU)"

export EL_RPC_URLS="$EL_RPC_URL"
export GUEST_BINARIES="zesu:$ZESU"
export GENESIS_FILE="$GENESIS_DIR/genesis.json"
export JWT_SECRET_FILE="$JWT_DIR/jwtsecret"
[ -n "$ENGINE_RPC_URL" ] && export ENGINE_RPC_URL
[ -n "$BLOCK" ] && export BLOCK_NUMBER="$BLOCK"

go run "$SCRIPT_DIR/."
