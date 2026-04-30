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

## zesu-zkvm tooling

Two CLI tools support offline benchmarking of the zesu-zkvm guest binary against mainnet block fixtures.

### `zesu-convert` — fixture to binary

Reads one JSON block fixture and writes the ziskemu-ready binary input to a file (or stdout).

```
zesu-convert <fixture.json> [output.bin]
zesu-convert <fixture.json> > output.bin
```

**Build**

```bash
go build -o zesu-convert ./cmd/zesu-convert
```

**Example**

```bash
./zesu-convert rpc_block_24758569.json block_24758569.bin
# stderr: ok: 4625040 bytes, block=24758569 txns=156
```

The output file can then be fed directly to `ziskemu`:

```bash
ziskemu-0.16.1 -X -e zesu-zisk -i block_24758569.bin
```

**Output format**

```
[u64 LE: payload_len]
[32 bytes: new_payload_request_root (zeros — placeholder)]
[u64 BE: block_rlp_len] [block RLP]
[u64: state_count]   [u64 len + node bytes] × N
[u64: codes_count]   [u64 len + code bytes] × N
[u64: keys_count]    [u64 len + key bytes]  × N
[u64: headers_count] [u64 len + header RLP] × N
[u64: pubkeys_count] [u64 len + 64-byte pubkey] × N
[0–7 zero bytes: alignment padding to multiple of 8]
```

The block RLP and public keys are derived automatically from the JSON fixture — no pre-processing needed.

---

### `bench` — batch benchmark runner

Runs a directory of JSON fixtures through ziskemu in parallel and produces a terminal summary plus an interactive HTML report.

```
bench --fixtures <dir> --elf <path> [--ziskemu <path>] [--jobs N] [--report <path>]
```

| Flag | Default | Description |
|---|---|---|
| `--fixtures` | *(required)* | Directory containing `*.json` fixture files, or a single file |
| `--elf` | *(required)* | Path to the compiled `zesu-zisk` ELF binary |
| `--ziskemu` | `ziskemu-0.16.1` | Path to the ziskemu emulator binary |
| `--jobs` | `1` | Number of parallel ziskemu runs |
| `--report` | `bench_report.html` | Output path for the HTML report |

**Build**

```bash
go build -o bench ./bench
```

**Example**

```bash
./bench \
  --fixtures ~/ere-input-testing/blocks_500_mainnet_Q12026 \
  --elf ~/dev/stateless/zesu-zkvm/zig-out/bin/zesu-zisk \
  --ziskemu ~/dev/stateless/ziskemu-0.16.1 \
  --jobs 4 \
  --report bench_report.html
```

**Terminal output** (one line per block as it completes, then a summary table):

```
[  1/500] block 24758569  total=34217493280  (12.4s)
[  2/500] block 24758570  total=1842938100   (1.1s)
...

=== Results (500/500 blocks) ===
COMPONENT               MIN                P50                MAX                AVG
--------------------------------------------------------------------------------------------
BASE             293601280         293601280         293601280         293601280
MAIN              12345678          98765432         432109876         134567890
OPCODES            1234567          12345678          98765432          23456789
PRECOMPILES              0           1234567          23456789           2345678
MEMORY             1234567           3456789          12345678           4567890
TOTAL            308016092         409403756        860313275         458836527
```

**HTML report**

Opens in any browser. Contains the cost summary table, a line chart of total cost by block number, and a stacked bar chart breaking down BASE / MAIN / OPCODES / PRECOMPILES / MEMORY per block. Blocks that errored are listed in a collapsible table with the raw ziskemu output.

---

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
