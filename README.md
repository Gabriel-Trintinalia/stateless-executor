# stateless-executor

Tools and services for stateless block verification of Ethereum blocks against zkVM guest programs.

This repo contains two largely independent halves:

- **Live pipeline** (`main.go`, `pool/`, `pipeline/`, `runner/`, `store/`, `metrics/`) — watches a live Ethereum network for new blocks, fetches each block's RLP and execution witness, and runs one or more zkVM guests in real time, reporting via Grafana.
- **Offline tooling** (`bench/`, `cmd/zesu-convert/`, `cmd/zkevm-runner/`) — batch-runs JSON fixtures through ziskemu against a built `zesu-zkvm` ELF and produces benchmark reports / pass-fail tables.

> **Status**: the live pipeline still emits the legacy zevm-zisk RLP `StatelessInput` envelope and is therefore broken end-to-end against the current SSZ-only `zesu-zkvm` guest. The offline tooling has been migrated to SSZ.

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

- **stdin** — binary-encoded block input. The live pipeline currently emits the legacy zevm-zisk RLP `StatelessInput` envelope; the offline tools (`zesu-convert`, `bench`, `zkevm-runner`) emit the Amsterdam SSZ `SszStatelessInput` envelope consumed by the SSZ-only `zesu-zkvm` guest.
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

## zesu-zkvm tooling (offline, SSZ-only)

Three CLI tools that build SSZ inputs from JSON fixtures and run them through ziskemu against a `zesu-zkvm` guest ELF. All three target the Amsterdam-spec `SszStatelessInput` envelope; the legacy RLP path has been removed.

All three build into `./bin/`. Build everything in one go:

```bash
go build -o bin/zesu-convert ./cmd/zesu-convert
go build -o bin/bench        ./bench
go build -o bin/zkevm-runner ./cmd/zkevm-runner
```

### `zesu-convert` — fixture to binary

Reads one JSON block fixture and writes a zkVM-ready SSZ binary input to a file (or stdout).

```
zesu-convert [--ziskinput | --openvm-input] <fixture.json> [output.bin]
zesu-convert <fixture.json> > output.bin
```

| Flag | Description |
|---|---|
| *(none)* | Raw SSZ body with no framing (for inspection / native zesu) |
| `--ziskinput` | 8-byte LE length header + 8-byte alignment padding (ZisK format) |
| `--openvm-input` | Same framing as `--ziskinput` (ZisK and OpenVM formats are identical) |

**Example — ZisK**

```bash
./bin/zesu-convert --ziskinput rpc_block_24758569.json block_24758569.bin
# stderr: ok: <N> bytes, block=24758569 txns=156

ziskemu -X -e zesu-zisk -i block_24758569.bin
```

**Example — OpenVM**

```bash
./bin/zesu-convert --openvm-input rpc_block_24758569.json block_24758569.bin

zesu-openvm-runner zesu-openvm.elf block_24758569.bin -o out.bin
xxd out.bin   # [0..32] new_payload_request_root, [32] success, [33..41] chain_id LE
```

**Output format**

```
[u64 LE: ssz_content_len]
[SszStatelessInput bytes]
[0–7 zero bytes: alignment padding to multiple of 8]
```

Transactions, the SSZ container layout, and pre-computed values are derived from the JSON fixture — no pre-processing needed.

---

### `bench` — batch benchmark runner

Runs a directory of JSON fixtures through a zkVM emulator in parallel and produces a terminal summary plus an interactive HTML report. Supports both ZisK and OpenVM targets.

```
bench --fixtures <dir> --elf <path> [--target zisk|openvm] [--ziskemu <path>] [--runner <path>] [--jobs N] [--report <path>]
```

| Flag | Default | Description |
|---|---|---|
| `--fixtures` | *(required)* | Directory containing `*.json` fixture files, or a single file |
| `--elf` | *(required)* | Path to the compiled zkVM ELF binary |
| `--target` | `zisk` | zkVM target: `zisk` or `openvm` |
| `--ziskemu` | `ziskemu` | Path to the ziskemu binary (`zisk-0.17+`; used when `--target zisk`) |
| `--runner` | `zesu-openvm-runner` | Path to the OpenVM runner binary (used when `--target openvm`) |
| `--jobs` | `1` | Number of parallel emulator runs |
| `--report` | `bench_report.html` | Output path for the HTML report |

**Example — ZisK**

```bash
./bin/bench \
  --fixtures ~/blocks_500_mainnet_Q12026 \
  --elf ~/dev/zesu-zkvm/zisk/zig-out/bin/zesu-zisk \
  --jobs 4 \
  --report bench_zisk.html
```

**Example — OpenVM**

```bash
./bin/bench \
  --target openvm \
  --fixtures ~/blocks_500_mainnet_Q12026 \
  --elf ~/dev/zesu-zkvm/openvm/zig-out/bin/zesu-openvm \
  --runner ~/dev/zesu-zkvm/openvm/runner/target/release/zesu-openvm-runner \
  --jobs 2 \
  --report bench_openvm.html
```

**Terminal output** (one line per block, then a summary):

ZisK:
```
[  1/500] block 24758569  total=34217493280  (12.4s)

=== Results (500/500 blocks, 500/500 validated) ===
COMPONENT               MIN                P50                MAX                AVG
--------------------------------------------------------------------------------------------
BASE             293601280         ...
TOTAL            308016092         ...
```

OpenVM:
```
[  1/500] block 24758569  (3.2s)

=== Results (500/500 blocks, 500/500 validated) ===
ELAPSED (ms)            MIN                P50                MAX                AVG
--------------------------------------------------------------------------------------------
ELAPSED               1200               3200               9800               3400
```

**HTML report**

Opens in any browser. The target is shown as a badge in the report header.

- **ZisK**: circuit cost summary table, total-cost line chart, and stacked breakdown (BASE / MAIN / OPCODES / PRECOMPILES / MEMORY) per block.
- **OpenVM**: elapsed-time line chart per block (no circuit cost breakdown — OpenVM emulation does not emit one).

Both targets include validation-failure and error tables.

---

### `zkevm-runner` — run zkevm spec-test fixtures

Reads zkevm blockchain-test JSON fixtures (one or more test cases, one or more blocks per case) and runs each block through ziskemu, comparing the guest's 41-byte SSZ output against the fixture's `statelessOutputBytes` and the success flag against `expectException`.

Pre-Amsterdam fixtures (no `statelessInputBytes`) are not supported — the SSZ-only guest needs the SSZ envelope from the fixture.

```
zkevm-runner --fixtures <dir> --elf <path> [--ziskemu <path>] [--jobs N] [--report <path>]
zkevm-runner --fixtures <dir> --dump-dir <out>   # encode .bin files without running ziskemu
```

| Flag | Default | Description |
|---|---|---|
| `--fixtures` | *(required)* | Directory containing zkevm `*.json` fixture files |
| `--elf` | *(required unless `--dump-dir` is set)* | Path to the compiled `zesu-zisk` ELF binary |
| `--ziskemu` | `ziskemu` | Path to the ziskemu emulator binary (zisk-0.17+; `ziskup --cpu` puts it on PATH) |
| `--jobs` | `1` | Number of parallel ziskemu runs |
| `--report` | *(optional)* | If set, writes an HTML report with pass/fail/error tables |
| `--dump-dir` | *(optional)* | Encode each block's input to `.bin` files in this directory and skip execution |

**Example**

```bash
./bin/zkevm-runner \
  --fixtures ~/dev/zesu/spec-tests/fixtures/zkevm/blockchain_tests \
  --elf ~/dev/zesu-zkvm/zisk/zig-out/bin/zesu-zisk \
  --jobs 4 \
  --report zkevm_report.html
```

The terminal prints one line per test block (`OK` / validation failure / error) followed by a summary; the optional HTML report tabulates each failing block with the expected vs actual SSZ output and the parsed error line from ziskemu.

---

## Project structure

```
stateless-executor/
├── main.go                # live-pipeline entry point (currently RLP-only — broken vs SSZ guest)
├── pool/                  # EL RPC pool — probes debug_executionWitness, polls heads
├── pipeline/              # fetches block RLP + witness, encodes binary input
├── runner/                # executes guest binaries, parses JSON output
├── store/                 # ring buffer + /results HTTP handler
├── metrics/               # Prometheus metrics
├── fixture/               # JSON-fixture parsing + SSZ encoding shared by the offline tools
├── bench/                 # offline benchmark runner
├── cmd/zesu-convert/      # one-shot fixture-to-bin converter
├── cmd/zkevm-runner/      # zkevm spec-test fixture runner
└── Dockerfile
```
