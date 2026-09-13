# AGENTS.md

Instructions for AI coding agents working in this repository.

## Quick Start

``` bash
go mod download
task build   # compile
task test    # run tests (coverage gated)
task lint    # lint all files
```

## Required Workflow

Always run these before considering work complete:

``` bash
go build ./... && task lint && task test
```

All three must pass with zero errors.

## Toolchain

Lint, format, and environment checks come from
[go-canon](https://github.com/SynthLuvr/go-canon) — one meta-tool that
owns every gate and preset, pinned in the `go.mod` `tool` block.

- Run tooling through `task <task>` (`task lint`, `task format`), which
  calls `go tool go-canon <command>`; never install or invoke bare tool
  binaries.
- `go tool go-canon lint` / `go tool go-canon format` accept path
  arguments and `--fast` (skips govulncheck, dupl, and the markdown
  gate).
- `go tool go-canon doctor` diagnoses toolchain/environment problems.
- Markdown files must be byte-identical to `pandoc --eol=lf -t gfm`
  output; fix drift with `task format`.
- Tool, rule, and preset changes belong in go-canon — bump its version
  in the `tool` block to pick them up. Do not add per-step tool scripts
  back here; local knobs go in `go-canon.toml`.

## Coding Conventions (Enforced)

These are **not** preferences — the toolchain will fail if you violate
them:

### gofumpt formatting

Formatting is not a debate; `golangci-lint fmt` (gofumpt + gci) owns it.

### Import grouping (gci)

Standard library, then external, then this module — enforced and
auto-fixed.

### Modern idioms

`for i := range n` (not the 3-clause loop), `slices`/`maps`/`min`/`max`
builtins, `strconv` instead of single-value `fmt.Sprintf` (modernize +
intrange + perfsprint).

``` go
// ❌ Wrong
for i := 0; i < len(list); i++ { ... }
s := fmt.Sprintf("%d", n)

// ✅ Right
for i := range len(list) { ... }
s := strconv.Itoa(n)
```

### Errors are values

Check every error (errcheck), wrap with `%w` at package boundaries
(wrapcheck), compare with `errors.Is`/`errors.As` (errorlint).

### Document exported symbols

Every exported symbol and package carries a doc comment (revive), the
docstring-discipline analog.

### Table-driven tests

Tests are co-located (`*_test.go`), table-driven where it helps, with
`t.Parallel()` where possible; total statement coverage stays at or
above 80%.

## Formatting

If the linter complains about formatting, run:

``` bash
task format
```

This runs four steps in order (via `go-canon format`):

1.  modernize `-fix` — idiom codemods
2.  golangci-lint fmt — gofumpt + gci
3.  `go mod tidy`
4.  pandoc — normalizes markdown to GFM

## Project Structure

- Source lives at the module root and in packages (`internal/` for
  private ones) — **no `src/` directory** (GOPATH-era baggage)
- Tests are co-located next to the code they test, not in a separate
  tree — that is how `go test` discovers them
- `go build ./...` is the type check; there is no separate type-check
  step
