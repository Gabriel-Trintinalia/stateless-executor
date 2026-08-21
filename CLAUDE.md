# stateless-executor — Claude context

## Running the benchmark

Build the ELF first (from `zesu-zkvm/zisk`), then run the bench tool.

```bash
# 1. Build the Zisk guest ELF (Rust lib + Zig binary)
cd /Users/gtrintinalia/Development/eth-proofs/zesu-zkvm/zisk && make

# 2. Run the benchmark (from stateless-executor root)
cd /Users/gtrintinalia/Development/eth-proofs/stateless-executor && \
go run ./bench \
  --fixtures /Users/gtrintinalia/Development/eth-proofs/blocks_500_mainnet_Q12026 \
  --elf /Users/gtrintinalia/Development/eth-proofs/zesu-zkvm/zisk/zig-out/bin/zesu-zisk \
  --zkvmPath ziskemu \
  --jobs 8 \
  --report bench_report.html \
  --csv bench_report.csv
```

### Fixture formats

`bench` accepts either format and detects it per file, so the same invocation
works for both:

- **corpus** — the flat one-block-per-file set, `blocks_500_mainnet_Q12026`
  (500 mainnet blocks, Q1 2026, 5.8 GB).
- **zkevm** — EEST `blockchain_tests` trees, where one file holds many named test
  cases of one or more blocks each, carrying a pre-encoded `statelessInputBytes`.
  The benchmark set (`fixtures_benchmark/blockchain_tests/for_amsterdam_at_{0010M,0030M,0060M}`)
  is 339 files / 3.7 GB / 7,390 blocks, and stresses individual opcodes at fixed
  gas — the sharpest signal available for guest cost per gas.

Discovery recurses and prunes dot-directories, so pointing `--fixtures` at a
spec-tests tree does not trip over its 166 MB `.meta/index.json`.

```bash
# Census only: what would run, per format and per suite. No emulator, seconds.
go run ./bench --fixtures ~/Downloads/fixtures_benchmark --dry-run

# Cost-measure the 60M-gas benchmark fixtures.
go run ./bench \
  --fixtures ~/Downloads/fixtures_benchmark/blockchain_tests/for_amsterdam_at_0060M \
  --elf ~/Development/eth-proofs/zesu-zkvm/zisk/zig-out/bin/zesu-zisk \
  --jobs 8 --report bench_60M.html --csv bench_60M.csv
```

### Notes

- `make` in `zesu-zkvm/zisk` only rebuilds the Zig ELF if the Rust lib (`lib/libziskos_staticlib.a`) is already present. Use `make clean-lib && make` to force a full rebuild.
- `--jobs 4` runs 4 **files** in parallel; blocks within a file run in sequence. Adjust to CPU count.
  Peak memory is bounded by roughly 2.6× the size of the files in flight — about 4.7 GB at `--jobs 8`
  over the benchmark tree, whose largest single fixture is 550 MB.
- `--report` produces an HTML report; `--csv` writes per-unit stats. The CSV's first 15 columns are
  unchanged from the archived `bench_*.csv` runs; `steps`, `suite` and `label` are appended after them.
  `suite` is the fixture's directory relative to `--fixtures`, which for the benchmark trees gives you
  the gas tier.
- Above 1,500 units the report replaces its per-unit charts with a cost-distribution curve and
  per-suite medians, and says so.
- `--maxSteps N` passes `-n` to the emulator. A run that reaches its step cap is reported as an error,
  never a pass: the emulator breaks out of its loop silently on `max_steps`, so a capped run otherwise
  looks like a cheap success with a truncated cost table. Detection also applies at ziskemu's own
  default cap of 68719476735.
- Validation differs by format and the rules are kept separate: corpus compares the guest's success
  against the fixture's `success` flag; zkevm compares the SSZ output bytes and never consults
  `expectException`. A zkevm block with no expected output is reported as *unverified* — measured but
  not validated — rather than an unconditional pass.
- `--target openvm` supports corpus fixtures only.
- `ziskemu` must be on `$PATH` (from the zisk toolchain install).
- `cmd/zkevm-runner` remains the correctness-only tool over the same zkevm fixtures. It shares the
  emulator invocation with `bench` (package `emu`) but keeps its own verdict code, so the two can be
  cross-checked against each other.
