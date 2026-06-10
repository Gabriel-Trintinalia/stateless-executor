# stateless-executor

Fetches blocks and execution witnesses from an Ethereum EL node, encodes them as Amsterdam SSZ `SszStatelessInput`, and runs one or more zkVM guest binaries to verify each block.

This repo contains two largely independent halves:

- **Live pipeline** (`main.go`, `pool/`, `pipeline/`, `runner/`, `store/`, `metrics/`) — watches a live Ethereum network, fetches each block's RLP, execution witness, and BAL, encodes them as `SszStatelessInput`, and runs guest binaries in real time.
- **Offline tooling** (`bench/`, `cmd/zesu-convert/`, `cmd/zkevm-runner/`) — batch-runs JSON fixtures through ziskemu against a built `zesu-zkvm` ELF and produces benchmark reports / pass-fail tables.

## Running against a live network

### Prerequisites

- Go 1.24+
- A running Ethereum EL node that supports `debug_getRawBlock`, `debug_executionWitness`, `eth_getBlockAccessList`, and `engine_getPayloadBodiesByHashV2`
- The `zesu` guest binary (built from the [zesu repo](https://github.com/Gabriel-Trintinalia/zesu))
- The network's `genesis.json`
- The engine API JWT secret

### Quick start with `dev.sh`

`dev.sh` starts a local Besu+Lighthouse devnet and runs the executor against it in one command:

```bash
./dev.sh --zesu /path/to/zesu
```

**Options:**

| Flag | Description |
|---|---|
| `--zesu <path>` | Path to the zesu guest binary (required) |
| `--block <number>` | Verify a single block and exit (omit for live mode) |
| `--enclave <name>` | Kurtosis enclave name (default: `stateless-devnet`) |

**Examples:**

```bash
# Live mode — verify every block as it arrives
./dev.sh --zesu ./path/to/zesu

# One-shot — verify block 42 and exit (exit code 0 = valid, 1 = fail/error)
./dev.sh --zesu ./path/to/zesu --block 42
```

### Running manually

If you already have an EL node running:

```bash
# Download genesis from a running enclave (if using the devnet)
kurtosis files download <enclave> el_cl_genesis_data /tmp/genesis-data
kurtosis files download <enclave> jwt_file /tmp/jwt-data

# Run in live mode
EL_RPC_URLS=http://127.0.0.1:<rpc-port> \
ENGINE_RPC_URL=http://127.0.0.1:<engine-port> \
JWT_SECRET_FILE=/tmp/jwt-data/jwtsecret \
GUEST_BINARIES=zesu:/path/to/zesu \
GENESIS_FILE=/tmp/genesis-data/genesis.json \
go run .

# Verify a single block
EL_RPC_URLS=http://127.0.0.1:<rpc-port> \
ENGINE_RPC_URL=http://127.0.0.1:<engine-port> \
JWT_SECRET_FILE=/tmp/jwt-data/jwtsecret \
GUEST_BINARIES=zesu:/path/to/zesu \
GENESIS_FILE=/tmp/genesis-data/genesis.json \
BLOCK_NUMBER=42 \
go run .
```

### Environment variables

| Variable | Required | Description |
|---|---|---|
| `EL_RPC_URLS` | ✓ | Comma-separated EL JSON-RPC endpoints |
| `GUEST_BINARIES` | ✓ | `name:/path/to/binary[,name2:/path/to/binary2]` |
| `GENESIS_FILE` | ✓ | Path to `genesis.json` (for chain config / fork detection) |
| `ENGINE_RPC_URL` | | Engine API endpoint (required for BAL on Amsterdam+) |
| `JWT_SECRET_FILE` | | Path to JWT secret for engine API auth |
| `BLOCK_NUMBER` | | If set, verify this single block and exit |
| `VERBOSE` | | Set to `true` to log raw witness headers and `statelessInputBytes` |
| `LISTEN_ADDR` | | HTTP listen address (default `:8080`) |

## HTTP API

| Endpoint | Description |
|---|---|
| `GET /results` | JSON array of the last 1000 verification results |
| `GET /metrics` | Prometheus metrics |

### Result schema

```json
{
  "block": 42,
  "guest": "zesu",
  "witness_from": "127.0.0.1",
  "valid": true,
  "tx_count": 12,
  "gas_used": 847293,
  "duration_ms": 83,
  "log": "...",
  "error": "..."
}
```

## Guest contract

- **stdin** — raw SSZ `SszStatelessInput` (schema_id `0x0001`, no framing)
- **stdout** — binary `SszStatelessValidationResult` (byte 32 = `successful_validation`)
- **stderr** — informational, shown in the `log` field

## Prometheus metrics

| Metric | Labels |
|---|---|
| `stateless_block_verified_total` | `guest`, `result` (`ok` / `fail` / `error`) |
| `stateless_verification_duration_ms` | `guest` |
| `stateless_el_pool_size` | — |
| `stateless_block_height` | — |

## zesu-zkvm tooling (offline, SSZ-only)

Three CLI tools that build SSZ inputs from JSON fixtures and run them through ziskemu. Build everything in one go:

```bash
go build -o bin/zesu-convert ./cmd/zesu-convert
go build -o bin/bench        ./bench
go build -o bin/zkevm-runner ./cmd/zkevm-runner
```

### `zesu-convert` — fixture to binary

```
zesu-convert [--zkvm-input] <fixture.json> [output.bin]
```

### `bench` — batch benchmark runner

```
bench --fixtures <dir> --elf <path> [--target zisk|openvm] [--jobs N] [--report <path>]
```

### `zkevm-runner` — zkevm spec-test fixtures

```
zkevm-runner --fixtures <dir> --elf <path> [--jobs N] [--report <path>]
```

## Project structure

```
stateless-executor/
├── main.go                # live-pipeline entry point
├── dev.sh                 # start devnet + run executor
├── devnet.yaml            # Besu+Lighthouse devnet config
├── run.sh                 # upload local artifacts then run a command
├── pool/                  # EL RPC pool
├── pipeline/              # fetch block + witness + BAL, encode SSZ input
├── runner/                # execute guest binaries, parse binary output
├── store/                 # ring buffer + /results HTTP handler
├── metrics/               # Prometheus metrics
├── fixture/               # SSZ encoding + genesis chain config
├── bench/                 # offline benchmark runner
├── cmd/zesu-convert/      # one-shot fixture-to-bin converter
├── cmd/zkevm-runner/      # zkevm spec-test fixture runner
└── Dockerfile
```
