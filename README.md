# bellwether

Web service that looks up a fixed number of closing prices of a specific
stock, scaffolded from
[go-template](https://github.com/SynthLuvr/go-template) with the full
go-canon build, format, lint, test, coverage, and security toolchain.

**Status: scaffold.** The placeholder code builds, lints, and tests
green; the service itself has not been implemented yet.

## Planned behavior

- `GET /` — the last `NDAYS` trading days of closing prices for `SYMBOL`
  (Alpha Vantage `TIME_SERIES_DAILY`, switched to `outputsize=full` when
  `NDAYS > 100`), plus the average close over those days, newest first,
  with `Cache-Control: public, max-age=3600`.
- `GET /health` — liveness probe, `Cache-Control: no-store`, never
  contacts Alpha Vantage.
- In-memory one-hour cache with single-flight; failed upstream calls are
  never cached.
- Errors map to `404`/`429`/`500`/`502`/`503` JSON bodies, never cached;
  the API key never appears in logs.
- Graceful shutdown on `SIGTERM`/`SIGINT` with a 10 second grace period;
  a second signal exits immediately.
- Configuration from environment variables only: `SYMBOL`, `NDAYS`,
  `APIKEY` (all required), `PORT` (optional, default `3000`), validated
  at startup with fail-fast.

## Tech Stack

| Tool | Purpose |
|----|----|
| [Go modules](https://go.dev/ref/mod) | Package manager (`go.sum` is the lockfile) |
| [go-canon](https://github.com/SynthLuvr/go-canon) | The shared lint/format/test/doctor toolchain |
| [golangci-lint](https://golangci-lint.run) | Meta-linter (errcheck, govet, staticcheck, …) |
| [gofumpt](https://github.com/mvdan/gofumpt) + gci | Formatting and import order |
| [govulncheck](https://go.dev/blog/vuln) | SCA: symbol-level vulnerability reachability |
| stdlib `testing` | Table-driven tests, race detector, fuzzing |
| [go-cmp](https://github.com/google/go-cmp) | Semantic diffs for test assertions |
| [Task](https://taskfile.dev) | Task runner for the one-command UX |
| [pandoc](https://pandoc.org) | Markdown formatter (GFM); system dependency |

Everything above except pandoc is pinned in the `go.mod` `tool` block
and compiled locally by `go tool`.

## Prerequisites

- [Go](https://go.dev) 1.27+ (`.go-version` pins 1.27.1;
  `GOTOOLCHAIN=auto` upgrades automatically)
- [pandoc](https://pandoc.org) ≥ 3.10 — required by the markdown gate;
  `task doctor` verifies it

## Quick Start

``` bash
go mod download
task build   # compile
task test    # tests + coverage gate
```

## Tasks

| Task          | Description                                |
|---------------|--------------------------------------------|
| `task build`  | Compile all packages                       |
| `task lint`   | Full static pipeline via go-canon          |
| `task format` | Auto-format and auto-fix                   |
| `task test`   | Tests + coverage (80% statement threshold) |
| `task check`  | Lint plus tests                            |
| `task doctor` | Diagnose the environment                   |

## Project Structure

Go idiom wins over template-family symmetry: no `src/` directory, tests
are co-located (`*_test.go` next to the code), and `go build ./...` is
the type check. See `AGENTS.md` for the enforced conventions.

    ├── .github/workflows/     # CI (Ubuntu + Windows)
    ├── index.go               # Placeholder module — replaced by the service
    ├── index_test.go          # Co-located table-driven test
    ├── internal/              # Private packages, added as the service grows
    ├── go-canon.toml          # Local deltas over go-canon's presets
    ├── go.mod / go.sum        # Module + pinned tool block
    ├── Taskfile.yml           # build / lint / format / test / check / doctor
    └── .go-version            # Go version for asdf/gvm/mise

Planned `internal/` packages — `config`, `alphavantage`, `cache`, `app`,
`logger`, `shutdown` — have not been created yet.
