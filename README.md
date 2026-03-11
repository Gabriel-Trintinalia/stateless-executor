# stateless-executor

Continuous stateless block verification pipeline. Watches a live Ethereum network for new blocks, fetches each block's RLP and execution witness, and runs one or more zkVM guest programs against every block — comparing results in real time.

## How it works

```
EL node pool  ──►  fetch block RLP + witness  ──►  encode binary  ──►  guest binary  ──►  result
     │                                                                       │
  (round-robin                                                       (one per block,
   head polling)                                                      all guests in
                                                                       parallel)
```

Results are stored in a ring buffer (last 1000 blocks) and exposed over HTTP.

## HTTP API

| Endpoint | Description |
|---|---|
| `GET /metrics` | Prometheus metrics |
| `GET /results` | JSON array of the last 1000 verification results |

### Result schema

```json
{
  "block": 624,
  "guest": "zevm-stateless",
  "valid": true,
  "pre_state_root": "0x...",
  "post_state_root": "0x...",
  "receipts_root": "0x...",
  "duration_ms": 83,
  "timestamp": 1773183013
}
```

## Prometheus metrics

| Metric | Labels |
|---|---|
| `stateless_block_verified_total` | `guest`, `result` (ok / fail / error) |
| `stateless_verification_duration_ms` | `guest` |
| `stateless_el_pool_size` | — |
| `stateless_block_height` | — |

## Guest contract

The executor runs guest binaries as subprocesses:

- **stdin** — binary-encoded block input (`[u64 rlp_len][rlp][state][codes][keys][headers]`)
- **stdout** — one JSON line with the verification result
- **stderr** — informational, logged but not parsed

## Configuration

| Env var | Required | Description |
|---|---|---|
| `EL_RPC_URLS` | yes | Comma-separated list of EL JSON-RPC endpoints |
| `GUEST_BINARIES` | yes | Comma-separated list of `name:/path/to/binary` |
| `FORK_NAME` | no | Fork name passed to guests via `--fork` (e.g. `cancun`) |
| `LISTEN_ADDR` | no | HTTP listen address (default `:8080`) |

## Running locally

```bash
# Build a guest image (zevm-stateless)
cd /path/to/eth-proofs
docker build \
  -f zevm-stateless/Dockerfile.zevm_stateless \
  --build-context zevm=zevm \
  -t zevm-stateless:latest \
  zevm-stateless

# Extract the binary
docker create --name tmp zevm-stateless:latest
docker cp tmp:/out/bin/zevm-stateless ./zevm-stateless
docker rm tmp

# Run the executor
EL_RPC_URLS=http://localhost:8545 \
GUEST_BINARIES=zevm-stateless:./zevm-stateless \
go run .
```

## Kurtosis integration

The `kurtosis/launcher.star` module integrates with [ethereum-package](https://github.com/Gabriel-Trintinalia/ethereum-package).

The launcher pulls each guest image, extracts the binary via `plan.run_sh`, mounts it as a Kurtosis file artifact into the executor container, and passes the binary paths via `GUEST_BINARIES`. No Docker daemon is needed inside the container.

### network_params.yaml

```yaml
participants:
  - el_type: geth
    cl_type: lighthouse

additional_services:
  - stateless_executor

stateless_executor_params:
  image: ghcr.io/Gabriel-Trintinalia/stateless-executor:latest
  guests:
    - image: ghcr.io/eth-proofs/zevm-stateless:latest
      binary: /out/bin/zevm-stateless
    - image: ghcr.io/other-team/their-verifier:latest
      binary: /usr/bin/stateless-verify
```

### Run

```bash
kurtosis run --enclave my-testnet \
  github.com/Gabriel-Trintinalia/ethereum-package@feat/stateless-executor \
  --args-file network_params.yaml
```

## Project structure

```
stateless-executor/
├── main.go           # entry point, wiring
├── pool/             # EL RPC pool — probes debug_executionWitness, polls heads
├── pipeline/         # fetches block RLP + witness, encodes binary input
├── runner/           # executes guest binaries, parses JSON output
├── store/            # ring buffer + /results HTTP handler
├── metrics/          # Prometheus metrics
├── kurtosis/
│   ├── launcher.star # Kurtosis launcher module
│   └── grafana/
│       └── dashboard.json  # Grafana dashboard (verification rate + results table)
└── Dockerfile
```

## Grafana dashboard

`kurtosis/grafana/dashboard.json` provides two panels:

- **Verification rate** — `rate(stateless_block_verified_total{result="ok"}[1m])` per guest
- **Recent results table** — live feed from `/results` (requires the [Infinity datasource plugin](https://grafana.com/grafana/plugins/yesoreyeram-infinity-datasource/))
