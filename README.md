# stateless-executor

Continuous stateless block verification pipeline. Watches a live Ethereum network for new blocks, fetches each block's RLP and execution witness, and runs one or more zkVM guest programs against every block — reporting results in real time via Grafana.

## Quickstart

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/)
- [Kurtosis CLI](https://docs.kurtosis.com/install)

### 1. Create `kurtosis.yaml`

```yaml
additional_services:
  - stateless_executor

stateless_executor_params:
  guests:
    - image: "ghcr.io/gabriel-trintinalia/zevm-stateless:latest"
      binary: "/out/bin/zevm_stateless"
      name: "zevm"
```

### 2. Run

```bash
kurtosis run \
  github.com/Gabriel-Trintinalia/ethereum-package@feat/stateless-executor \
  --args-file kurtosis.yaml
```

Kurtosis prints a `stateless_executor_dashboard_url` at the end — open it in Grafana to see live verification results.

## Adding guest binaries

Any verifier that speaks the guest contract can be added alongside or instead of `zevm-stateless`:

```yaml
stateless_executor_params:
  guests:
    - image: "ghcr.io/gabriel-trintinalia/zevm-stateless:latest"
      binary: "/out/bin/zevm_stateless"
      name: "zevm"
    - image: "ghcr.io/other-team/their-verifier:latest"
      binary: "/usr/bin/stateless-verify"
      name: "other"
```

All guests run against every block in parallel and their results appear as separate rows in the dashboard table.

## Optional parameters

```yaml
stateless_executor_params:
  image: "ghcr.io/gabriel-trintinalia/stateless-executor:latest"  # override executor image
  fork_name: "cancun"                                              # passed to guests via --fork
  guests:
    - image: "..."
      binary: "..."
      name: "..."
```

## Guest contract

Guest binaries receive block data on **stdin** and write the result to **stdout**:

- **stdin** — binary-encoded block input: `[u64 rlp_len][rlp][state][codes][keys][headers]`
- **stdout** — one JSON line: `{"block": N, "valid": true}`
- **stderr** — informational, captured and shown in the `log` column

## HTTP API

| Endpoint | Description |
|---|---|
| `GET /results` | JSON array of the last 1000 verification results |
| `GET /metrics` | Prometheus metrics |

### Result schema

```json
{
  "block": 42,
  "guest": "zevm",
  "witness_from": "el-1-geth",
  "valid": true,
  "tx_count": 12,
  "gas_used": 847293,
  "duration_ms": 83,
  "log": "...",
  "error": "..."
}
```

## Prometheus metrics

| Metric | Labels |
|---|---|
| `stateless_block_verified_total` | `guest`, `result` (`ok` / `fail` / `error`) |
| `stateless_verification_duration_ms` | `guest` |
| `stateless_el_pool_size` | — |
| `stateless_block_height` | — |

## How it works

```
EL node pool  ──►  fetch block RLP + witness  ──►  encode binary  ──►  guest binary  ──►  result
     │                                                                       │
  (round-robin                                                       (one per block,
   head polling)                                                      all guests in
                                                                       parallel)
```

## Project structure

```
stateless-executor/
├── main.go       # entry point, wiring
├── pool/         # EL RPC pool — probes debug_executionWitness, polls heads
├── pipeline/     # fetches block RLP + witness, encodes binary input
├── runner/       # executes guest binaries, parses JSON output
├── store/        # ring buffer + /results HTTP handler
├── metrics/      # Prometheus metrics
└── Dockerfile
```
