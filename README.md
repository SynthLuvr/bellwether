# bellwether

A small web service that reports the last N daily closing prices — and
their average — for a single stock, pulled from Alpha Vantage’s
`TIME_SERIES_DAILY` endpoint.

It’s deliberately boring technology: Go’s standard library for
everything (HTTP, routing, retries, caching, logging, graceful
shutdown), with exactly one dependency beyond the stdlib —
[joho/godotenv](https://github.com/joho/godotenv), so you can keep
configuration in a `.env` file while developing. The interesting parts
are the operational choices: it stays inside Alpha Vantage’s free-tier
request quota no matter how much traffic arrives, it degrades gracefully
when the upstream fails, and it never leaks your API key.

## Contents

- [What it does](#what-it-does)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [HTTP API](#http-api)
- [Caching and the quota budget](#caching-and-the-quota-budget)
- [When the upstream misbehaves](#when-the-upstream-misbehaves)
- [Logging](#logging)
- [Graceful shutdown](#graceful-shutdown)
- [Docker](#docker)
- [Kubernetes](#kubernetes)
- [How it fails: a resilience tour](#how-it-fails-a-resilience-tour)
- [Development](#development)
- [Project layout](#project-layout)
- [License](#license)

## What it does

Two endpoints, no ceremony:

| Endpoint | What you get |
|----|----|
| `GET /` | The last `NDAYS` trading-day closes for `SYMBOL`, plus their average. |
| `GET /health` | A liveness check that never touches Alpha Vantage. |

One symbol per deployment (set at startup), end-of-day data only. The
whole thing compiles to a static binary that serves both endpoints from
a cache, so most requests never leave the pod.

## Quick start

You need:

- **Go 1.27+** — everything else (task runner, linters, formatters) is
  pinned in the `go.mod` tool block and compiled on demand by `go tool`,
  so you don’t install any of it yourself.
- **An Alpha Vantage API key** — grab a free one at
  <https://www.alphavantage.co/support/#api-key>. The free tier allows
  25 requests/day, which the service is designed to live within.
- Optionally, **Docker** to build the image, and **minikube** +
  **kubectl** to deploy it (covered in their own sections below).

Then:

``` bash
git clone git@github.com:SynthLuvr/bellwether.git
cd bellwether
```

Create a `.env` in the project root (it’s gitignored — the key never
goes near git):

``` bash
SYMBOL=MSFT
NDAYS=30
APIKEY=your-key-here
```

Start the service:

``` bash
go tool task start
# {"time":"2026-09-13T10:29:00.123456789Z","level":"INFO","msg":"listening","port":3000,"symbol":"MSFT","ndays":30,"cacheTtlSeconds":3600}
```

(The first `go tool` invocation compiles the pinned tools, so give it a
minute.)

Try it:

``` bash
curl http://localhost:3000/health
# {"status":"ok"}

curl http://localhost:3000/
```

`task start` is a thin wrapper over `go run .`. The `.env` file is
optional — real environment variables always win over it, so you can
also configure the service directly:

``` bash
SYMBOL=IBM NDAYS=5 APIKEY=your-key go tool task start
```

## Configuration

Everything comes from environment variables:

| Variable    | Required | Description                                          |
|-------------|----------|------------------------------------------------------|
| `SYMBOL`    | yes      | Stock symbol to look up (e.g. `MSFT`).               |
| `NDAYS`     | yes      | Number of trading days to return (positive integer). |
| `APIKEY`    | yes      | Alpha Vantage API key.                               |
| `PORT`      | no       | Listen port, 1–65535 (default `3000`).               |
| `CACHE_TTL` | no       | Series cache lifetime (default `1h`, at most `24h`). |

Validation is strict and happens at startup, in plain Go: if anything is
missing or malformed, the process prints every problem it found — one
line per variable, in declaration order — and exits nonzero. One bad
restart tells you about all of your typos, not just the first.

## HTTP API

### The `GET /` response

The last `NDAYS` trading days of closing prices plus the average closing
price over those days. Trading days only — market holidays and weekends
are skipped — sorted newest first:

``` json
{
  "symbol": "MSFT",
  "ndays": 5,
  "lastRefreshed": "2026-09-11",
  "averageClose": 494.674,
  "closes": [
    { "date": "2026-09-11", "close": 495.63 },
    { "date": "2026-09-10", "close": 492.44 },
    { "date": "2026-09-09", "close": 491.65 },
    { "date": "2026-09-08", "close": 493.95 },
    { "date": "2026-09-04", "close": 499.7 }
  ]
}
```

A few things worth knowing:

- If the daily series holds fewer entries than `NDAYS` (a recently
  listed stock, say), you get all available entries.
- Requests with `NDAYS > 100` switch to Alpha Vantage’s
  `outputsize=full`, so you can see past the compact 100-point window.
- **Partial trading day.** Queried while the US market is open, the
  newest row is the latest traded price of a session that hasn’t settled
  yet — not a real close. The service passes that row through as-is
  rather than trying to guess which session is still in progress (that
  needs the exchange calendar and timezone), because `lastRefreshed` —
  surfaced verbatim in every response — already tells you exactly how
  fresh the newest row is. Consequence: during market hours the average
  may include one intraday price. If you only want settled closes, drop
  the newest entry when its date equals `lastRefreshed`.
- Successful responses carry
  `Cache-Control: public, max-age=<CACHE_TTL>`. The payload is
  end-of-day data that changes at most once per trading day, so clients
  and CDNs may serve it from cache for exactly as long as the service
  itself would.

### The `GET /health` endpoint

Liveness probe, for other services and for orchestrators. Returns `200`
with a small JSON body and never contacts Alpha Vantage — it answers as
long as the process is up:

``` json
{ "status": "ok" }
```

Sent with `Cache-Control: no-store`, so probes always reach the process
itself, never a cache.

### Errors

All errors map to HTTP statuses with a JSON body:

| Status | Cause |
|----|----|
| `404` | Unknown path. |
| `429` | Alpha Vantage rate limit hit (free tier: 25 calls/day). The notice arrives in the legacy `Note` field or, as observed live, an `Information` body worded as a rate limit (“rate limit”, “call frequency”). |
| `500` | Unexpected internal error (e.g. a network-level failure reaching Alpha Vantage, or an upstream body that is not valid JSON). |
| `502` | Invalid symbol/key, upstream HTTP error, or bad payload. |
| `503` | Other Alpha Vantage informational notices (demo-key or premium-tier restrictions, etc.). |

Error responses are sent with `Cache-Control: no-store`: a failure is
never served past the cooldown window, and never from any cache.

Unhandled errors return a generic `500` JSON body; the error message and
stack are logged server-side and never sent to the client.

## Caching and the quota budget

The daily series behind `GET /` is cached in memory for `CACHE_TTL` (one
hour by default). Concurrent misses are collapsed into a single upstream
call (single-flight), so a traffic spike costs the same one Alpha
Vantage request as a lone visitor.

That makes the quota math simple: a fleet of `R` replicas makes at most
about `R × 24h / CACHE_TTL` Alpha Vantage requests per day, regardless
of client load. The Kubernetes manifest runs two replicas at
`CACHE_TTL=2h` — 24 refreshes/day, inside the free tier’s 25/day limit —
no matter how much traffic arrives. `NDAYS > 100` configurations benefit
most: the full history is downloaded once per TTL window instead of on
every request.

The corollary: **replicas × 24h / CACHE_TTL ≤ 25** is a
capacity-planning invariant. Change replicas or the TTL, re-check the
math.

## When the upstream misbehaves

Alpha Vantage is the one dependency that can actually fall over, so most
of the service’s care goes into failing well around it. The full
failure-mode analysis lives in [the resilience section
below](#how-it-fails-a-resilience-tour); the behavior in brief:

### Failure cooldown

A failed upstream call arms a 30-second cooldown. The request that
observed the failure pays the upstream exchange; requests inside the
window afterwards are answered without another attempt. Warm pods serve
the stale series instantly, cold pods fail fast with the recorded error
instead of waiting out another upstream timeout — so client traffic
cannot hammer a quota-exhausted or hard-down upstream once per request.
Once the cooldown elapses, the next request retries, so recovery is
detected within 30 seconds.

A loader that panics is treated the same way: the panic is converted
into an error (carrying the panicking stack) and handed to every caller
waiting on that flight, arming the cooldown rather than wedging the
cache permanently.

### Serving stale instead of failing

When the TTL lapses and the reload fails, the previous series is served
— for up to 24h after it was loaded — instead of failing the request: a
`200` with an `X-Data-Stale: true` header, `Cache-Control: no-store`
(degraded data must not sit in shared caches), and a `WARN` log line
carrying the upstream error.

End-of-day closes make this safe: the last completed close stays correct
until the next one exists. Past the 24h bound the mapped upstream error
(429/502/503/500) surfaces instead, so a long outage degrades loudly
rather than silently serving ever-older data.

A pod restart starts with a cold cache: its first request pays exactly
one upstream call (the quota price of a restart), and there is nothing
stale to serve until one reload has succeeded.

### Retries and timeouts

The Alpha Vantage client is standard library `net/http` plus an explicit
retry loop, tuned for a slow upstream on a tight rate limit:

- per-attempt timeout of 5s; a timeout is terminal (no retry), so a
  stalled upstream fails fast instead of silently doubling the wait;
- exactly one retry, after a ~300ms backoff, for transport-level
  failures only: network errors and HTTP 500/502/503/504;
- HTTP 429 is never retried, so a retry cannot re-hit a rate limit;
- a retried 503’s `Retry-After` (seconds or HTTP-date) is capped at 1s;
  other statuses ignore the header.

### Your API key stays secret

The key travels in the outbound URL’s query string, which makes it a
leak hazard in exactly two places, both plugged:

- **Logs.** Upstream URLs are never logged; request logs carry only the
  measured latency.
- **Error text.** Network errors are surfaced through a wrapper that
  redacts the URL and the key from the error text — every rendering (the
  full URL, the URL without its query, the key raw or URL-escaped) is
  scrubbed rather than assumed absent. The same scrubbing applies to
  client-facing error bodies: Alpha Vantage’s quota notices embed the
  API key in the message text, so surfaced messages have the key
  replaced with `[redacted]` before they reach a response. The
  underlying cause stays available for `errors.Is`/`errors.As`.

## Logging

One JSON object per line on stdout, via `log/slog` in its standard JSON
format (RFC 3339 timestamps, uppercase levels). A `listening` line at
startup, then one `request` line per HTTP request carrying the method,
path, response status, and total duration (`durationMs`).

For `GET /`, `upstreamMs` additionally times the series load — nothing
else: an Alpha Vantage round-trip on a cache miss, near-zero on a cache
hit (a failure is either a real upstream exchange or a near-zero
cooldown fast-fail, so a slow `upstreamMs` on an error really was an
upstream call).

Lines are logged at `INFO` for 2xx/3xx, `WARN` for 4xx, and `ERROR` for
5xx; unhandled errors additionally log the error message and stack. A
request served stale after a failed reload logs
`serving stale series after upstream failure` at `WARN` before its
(still `INFO`) request line. Even a panicking handler produces its
request line — logged as a `500` with `panicked` true — before net/http
recovers the panic and closes the connection.

## Graceful shutdown

`SIGTERM` and `SIGINT` (what a redeploy or Ctrl-C sends) trigger a
graceful shutdown: the service stops accepting new connections and waits
for in-flight requests to finish before exiting. Idle keep-alive
connections and requests still running after a 15-second grace period
are closed forcibly. A second signal skips the wait and exits
immediately.

If the listener dies outside of shutdown, the process logs the failure
and exits nonzero, so the supervisor restarts it.

## Docker

The service ships as a
[distroless](https://github.com/GoogleContainerTools/distroless) static
image: a `CGO_ENABLED=0` binary (~2 MB image) with CA certificates baked
in (Alpha Vantage is HTTPS), running as the unprivileged `nonroot` user.

Build and run it:

``` bash
docker build --tag bellwether .

docker run --publish 3000:3000 \
  --env SYMBOL=IBM --env NDAYS=30 --env APIKEY=your-key \
  bellwether
```

A few details you’d otherwise have to discover the hard way:

- Configuration comes exclusively from environment variables (the table
  above); no `.env` file is baked into the image.
- The binary is PID 1 — it installs its own signal handlers and spawns
  no children, so no init shim is needed. `docker stop` sends `SIGTERM`
  straight to the binary, so the graceful shutdown path runs.
- Distroless ships no shell or wget, so the Docker `HEALTHCHECK` runs
  the binary itself (`bellwether healthcheck`), which probes its own
  `GET /health`; `docker ps` reports `(healthy)` once the service is
  answering.

### Prebuilt images

A publish workflow pushes the image to [GitHub Container
Registry](https://github.com/features/packages) for every update that
lands on `main` — direct pushes and merged pull requests alike:

``` bash
docker pull ghcr.io/synthluvr/bellwether:latest
```

`latest` always points at the most recent `main` build, and each publish
is additionally tagged `sha-<commit>` so deployments can pin an exact
build. The first publish creates the package as private; make it public
from its package settings page if anyone outside the repository should
be able to pull it.

## Kubernetes

[`k8s/`](k8s/) deploys the service on a local
[minikube](https://minikube.sigs.k8s.io/) cluster behind an nginx
ingress at `http://bellwether.local`, fronted by a `ClusterIP` Service,
all inside a dedicated `bellwether` namespace.

Configuration is split by sensitivity:

- `bellwether-config` (ConfigMap): `SYMBOL=MSFT`, `NDAYS=7`,
  `CACHE_TTL=2h` (two replicas at a 2h TTL keep the fleet at 24
  refreshes/day, inside the free-tier quota — see the quota math above).
- `bellwether-secret` (Secret): `APIKEY`, created from the gitignored
  `.env` — its value is never committed to the repository.

The Deployment runs two replicas behind a `PodDisruptionBudget`
(`minAvailable: 1`), so node drains and voluntary evictions always leave
one pod serving, and rolls with `maxUnavailable: 0` + `maxSurge: 1`, so
redeploys and the key rotation below are zero-downtime.
[`k8s/kustomization.yaml`](k8s/kustomization.yaml) packages the
manifests — and is where deployments pin an exact build via its `images`
transformer instead of tracking `latest`.

### One-time setup

``` bash
minikube start
minikube addons enable ingress
```

### Deploy

``` bash
docker build -t ghcr.io/synthluvr/bellwether:latest .
minikube image load ghcr.io/synthluvr/bellwether:latest

# .env also holds SYMBOL/NDAYS, but the ConfigMap supplies those —
# extract only the API key, into the namespace the manifests deploy
# in (created ahead of the Secret so the first pods never wait on a
# missing one; apply -k adopts it).
kubectl create namespace bellwether
kubectl -n bellwether create secret generic bellwether-secret \
  --from-literal=APIKEY="$(sed -n 's/^APIKEY=//p' .env)"

kubectl apply -k k8s
kubectl rollout status -n bellwether deployment/bellwether
```

### Reach it

Point `bellwether.local` at the minikube IP by appending the printed
line to `/etc/hosts`, then try the ingress:

``` bash
echo "$(minikube ip) bellwether.local"
curl http://bellwether.local/health
curl http://bellwether.local/
```

Without editing `/etc/hosts`, a one-off request works too:

``` bash
curl --resolve bellwether.local:80:"$(minikube ip)" http://bellwether.local/
```

**Docker Desktop caveat.** With Docker Desktop — on Windows (WSL2
backend) or macOS — the `minikube ip` address lives inside the Docker
Desktop VM and is not routable from the host, so both curls above time
out. The cluster itself is unaffected; reach the ingress through the API
server instead, keeping the host header that selects the rule:

``` bash
kubectl port-forward -n ingress-nginx svc/ingress-nginx-controller 8080:80
curl -H "Host: bellwether.local" http://127.0.0.1:8080/health
curl -H "Host: bellwether.local" http://127.0.0.1:8080/
```

### Rotate the API key

Environment variables are captured at pod start, so replacing the Secret
alone leaves running pods on the old key; roll them afterwards and the
`maxUnavailable: 0` rollout keeps one pod serving throughout:

``` bash
kubectl -n bellwether create secret generic bellwether-secret \
  --from-literal=APIKEY=<new-key> --dry-run=client -o yaml | kubectl apply -f -
kubectl -n bellwether rollout restart deployment/bellwether
kubectl rollout status -n bellwether deployment/bellwether
```

### Tear it down

``` bash
kubectl -n bellwether delete secret bellwether-secret
kubectl delete -k k8s
```

(Deleting the `bellwether` namespace — part of `kubectl delete -k` —
takes the Secret with it; deleting it first is just tidiness.)

## How it fails: a resilience tour

How this service fails, what already guards each failure, and what I
would do next, in priority order, with tradeoffs. Nothing here is
aspirational hand-waving about “use monitoring” — every failure mode
below is specific to what this service actually depends on: one external
quote API on a 25 requests/day free-tier key, and end-of-day data whose
value comes from being immutable per date.

### Service objectives

These shape every choice that follows:

- **Availability:** `GET /` answers `200` for 99.9%+ of requests, pod
  restarts and redeploys included. Data availability and service
  availability are treated separately: a degraded answer (`200` +
  `X-Data-Stale: true`) beats no answer.
- **Latency:** p95 well under 100ms — the series cache means the Alpha
  Vantage round trip is paid once per TTL window, not per request. Only
  the unlucky first request after expiry waits on the upstream (bounded
  by its 5s timeout).
- **Freshness:** within one `CACHE_TTL` of upstream in normal operation;
  at most 24h behind under total upstream failure — and always
  *self-describing*: `lastRefreshed` and `X-Data-Stale` tell the client
  exactly what it got.
- **Quota:** the whole fleet stays inside 25 requests/day per key — the
  `replicas × 24h / CACHE_TTL ≤ 25` invariant is treated as a
  capacity-planning constraint, not an accident.

### Failure modes and what happens today

| Failure | Detected how | Behavior |
|----|----|----|
| Quota exhausted (200 + `Note`, or a rate-limit-worded `Information` body) | payload parse | `429` to clients; warm pods serve stale instead |
| Invalid symbol/key (200 + `Error Message`) | payload parse | `502` with the upstream message |
| Other informational notice (200 + `Information`: demo-key or premium-tier restrictions) | payload parse | `503` |
| Upstream 5xx | status check | one retry after 300ms (Retry-After honored on 503, capped 1s), then `502`; warm pods serve stale |
| Upstream stall | 5s per-attempt timeout | terminal — not retried — so the request fails fast |
| Network error / TLS / DNS | transport error | one retry, then `500` with redacted error; warm pods serve stale |
| Upstream down entirely | any of the above | stale serving for up to 24h past load, then the mapped error — degraded loudly, never silently aging; one upstream exchange per 30s cooldown window, not one per request |
| Pod restart | — | cold cache: the first request pays exactly one upstream call (the quota price of a restart), then cached again |
| Node drain / eviction | PDB | `minAvailable: 1` with two replicas keeps one pod serving |
| Redeploy / key rotation | rollout strategy | `maxUnavailable: 0`, `maxSurge: 1` — never below one healthy replica |
| Traffic spike | — | single-flight collapses concurrent misses into one upstream call; the upstream request rate is TTL-bounded regardless of client load |
| Misconfiguration | startup validation | fail-fast listing every problem; the pod CrashLoops visibly instead of serving garbage |
| Leaked key in logs/errors | redaction wrapper | URL and key scrubbed from every surfaced error text; request logs never carry URLs |

The unifying design rule: **the upstream is the only component allowed
to fail loudly, and even it degrades gracefully when we have data to
serve.** Everything the service controls (process, config, rollout) is
arranged to fail *before* traffic arrives (validation, probes) or
*without dropping requests* (graceful shutdown, zero-downtime rolls).

### What is in place today

- TTL cache + single-flight, quota-budgeted fleet-wide (`cache.go`,
  `CACHE_TTL`).
- Bounded serve-stale: `200` + `X-Data-Stale` + `no-store` for up to 24h
  past load when a reload fails (`cache.go`, `app.go`).
- Failure cooldown: a failed load fast-fails for 30s — warm pods serve
  stale instantly, cold pods return the recorded error — so a
  quota-exhausted or hard-down upstream costs one exchange per window,
  not one per request; recovery is detected within 30s (`cache.go`).
- Retry policy tuned for a rate-limited upstream: exactly one retry,
  5xx/transport only; 429 never retried; terminal timeouts
  (`alphavantage.go`).
- Alpha Vantage’s 200-with-error-body convention mapped to real statuses
  (429/502/503) instead of `NaN` averages (`parseDailySeries`).
- Graceful SIGTERM/SIGINT shutdown with a 15s grace period
  (`shutdown.go`) — what `kubectl rollout` and `docker stop` actually
  send.
- `/health` liveness + readiness probes, Docker `HEALTHCHECK`, non-root
  distroless image.
- Two replicas, PDB, zero-downtime rollout; key rotation runbook in the
  [Kubernetes section](#rotate-the-api-key) (`k8s/manifest.yaml`).
- Structured logs with `upstreamMs`, stale-serve warnings, and key/URL
  redaction; startup `configError` reports every problem at once.

### What I would do next

Ordered by (user-visible risk removed) ÷ effort.

#### P0 — Metrics and alerting

Everything above is a design claim until it is measured. Add `/metrics`
(Prometheus text format is a few hundred lines with the stdlib, or
`client_golang`): request count by status, upstream latency histogram,
cache hit ratio, a **stale-serve counter**, and a quota-429 counter.
Then two alerts: `stale serves > 0 for 15m` (upstream unhealthy — this
is the SLO signal for data freshness) and `5xx ratio > 1% over 10m`
(availability burn). *Tradeoff:* a metrics dependency and the usual
cardinality discipline; nothing else on this list can be verified
without it, which is why it is first.

#### P1 — Shared cache across replicas

Per-pod caches multiply the quota budget (`R × 24h / TTL`) and stay cold
after every restart. A small Redis (or even a ConfigMap-backed “last
good payload” object) shared by the pods gives one fleet-wide refresh
budget, warm pods after restarts (the restart quota cost drops to zero),
and a natural place for the stale fallback to survive redeploys.
*Tradeoff:* a stateful dependency for a stateless service — worth it
past ~2 replicas or the day the quota math gets tight; skip it at
current scale.

#### P1 — A measured breaker window

The 30s failure cooldown already delivers the breaker’s core property: a
hard-down or quota-exhausted upstream costs one exchange per window
instead of one per TTL-expired request, and inside the window
degradation is instant — a stale serve or the recorded error, never a
5s-per-request timeout wait. What is left is tuning the window from
measurement: an error-rate-driven open/half-open state machine that
widens the window for long outages (quota exhausted until midnight does
not need probing every 30s) and keeps it short for transient blips.
*Tradeoff:* a second mechanism on top of the cooldown, only worth adding
once the P0 metrics exist to justify its thresholds.

#### P2 — Scaling and load control

- **HPA:** CPU is the wrong signal for a cache-serving workload (hits
  idle the process). Scale on request rate (custom metric or KEDA).
  Re-check the quota invariant first — with per-pod caches, more
  replicas burn more quota, so the shared cache above is the
  prerequisite.
- **Per-client rate limiting / load shedding** at the ingress or in a
  middleware: one abusive client should not consume the p95 budget;
  `429` with `Retry-After` is the correct answer to unbounded clients.
- **500 symbols instead of 1:** cache per symbol (the current loader is
  single-symbol by construction), and face the quota math: 500 symbols ×
  daily refresh is far past 25/day — options are key pooling (N keys,
  one budget each), a paid tier, or a batch/consolidated feed. The
  architecture change is small; the *contract* with the upstream is the
  real cost.

#### P2 — Secret lifecycle

The current rotation runbook (update Secret, `rollout restart`,
zero-downtime) is two commands but two *manual* commands and a repo
checkout. External Secrets Operator against a real secret manager
(rotation happens where keys are owned) with a sync period turns
rotation into a non-event and removes the last human step.
*Non-negotiables that already hold:* the key is never logged (redaction
wrapper) and never committed (gitignored `.env`, Secret created out of
band).

### Playbooks

**Alpha Vantage is down for 10 minutes.** Warm pods: the first request
after TTL expiry attempts one reload, fails, and serves stale `200`s
with `X-Data-Stale: true`; requests inside the 30s cooldown afterwards
serve stale without another attempt — clients see correct, slightly old
data and can tell it is old. Cold pods (just restarted): `502/500` until
their first successful load. First response: nothing — this is the
system working; watch the stale-serve metric, page if it exceeds 15m.

**Quota exhausted at 3pm.** Pods with warm caches serve stale; cold pods
return `429` (the daily-quota notice maps to `429` whether it arrives in
`Note` or a rate-limit-worded `Information`). Response: raise
`CACHE_TTL` in the ConfigMap (rolling update applies it with zero
downtime), which divides the refresh rate; or rotate to a fresh key.
Prevention: the `replicas × 24h / TTL ≤ 25` invariant checked at review
time.

**Key rotation.** See the runbook in the [Kubernetes
section](#rotate-the-api-key): replace the Secret, `rollout restart`,
`maxUnavailable: 0` keeps service continuous; pods are created with the
new key, old pods drain with the old one. No repo change, no code
change.

### Data correctness notes

- **Partial trading day:** during US market hours the newest row is an
  in-progress session, not a settled close. The service passes it
  through and surfaces `lastRefreshed` so clients can detect it; the
  reasoning is in [the `GET /` section](#the-get-response).
- **Immutability assumption:** daily closes are treated as immutable per
  date, which is what makes long TTLs and serve-stale safe. The residual
  risk — upstream revising a close — is bounded by the TTL window in
  normal operation and accepted during degradation (a stale close is
  *correct as of its date*; `lastRefreshed` says which date).
- **The 24h stale bound:** a Friday-evening close would exceed it by
  Monday pre-open, so a weekend-long outage serves errors instead of
  weekend-old data. That is deliberate — bounded, visible degradation
  over unbounded silent staleness — and tunable in one constant if the
  product prefers the other tradeoff.

## Development

``` bash
go tool task build   # compile (the type check)
go tool task test    # tests + coverage gate
go tool task lint    # lint all files
go tool task format  # format all files
go tool task start   # run the service
go tool task doctor  # diagnose the environment
```

(If you have [Task](https://taskfile.dev) installed, plain `task <name>`
works too — the Taskfile is just a thin layer over `go tool`.)

Lint, format, and coverage gates come from
[go-canon](https://github.com/SynthLuvr/go-canon), a meta-tool pinned in
the `go.mod` tool block: one version bump picks up every rule change,
and nobody installs linters by hand. Markdown files are normalized
through pandoc as part of the same gate.

### Tests

`task test` runs the suite through `go test -race` with a coverage gate.
A few things make it pleasant to work in:

- Every HTTP test stands up the real application on an OS-assigned
  `httptest.Server` and exercises it over HTTP.
- Upstream traffic is served by a second `httptest.Server` standing in
  for Alpha Vantage — the client’s base URL is injectable, so the suite
  stays hermetic: no external calls, no module mocks, the application
  code runs unmodified.
- TTL behavior is deterministic through the cache’s injectable clock,
  and shutdown timing through injectable timers.
- Coverage is gated at 99% statements: everything except `main()` — the
  composition root, which `go test` cannot execute because importing the
  package starts a listener.

### CI

Two workflows live in [`.github/workflows/`](.github/workflows/):

- **CI** (every pull request) runs the full lint + test pipeline, then
  builds the image and smoke-tests it for real: container started,
  `/health` polled until it answers, the binary verified as PID 1, and
  graceful shutdown on SIGTERM verified from the logs.
- **Publish** (every push to `main`, merges included) builds and pushes
  the image to GHCR as described in [the Docker
  section](#prebuilt-images).

## Project layout

Everything is a `package main` at the module root — it’s one small
service, and the files are split by concern:

| Path | What lives there |
|----|----|
| `main.go` | Composition root: env, `.env`, signals, listener wiring. |
| `app.go` | Routes, handlers, response shaping, request logging middleware. |
| `alphavantage.go` | Upstream client: fetch, retry policy, parsing, error mapping, redaction. |
| `cache.go` | The cached loader: TTL, single-flight, failure cooldown, serve-stale. |
| `config.go` | Environment variable parsing and validation. |
| `shutdown.go` | SIGTERM/SIGINT handling and the graceful drain. |
| `healthcheck.go` | The `bellwether healthcheck` subcommand behind the Docker HEALTHCHECK. |
| `logger.go` | The structured logger seam over `log/slog`. |
| `k8s/` | Kustomize manifests: namespace, config, deployment, PDB, service, ingress. |
| `.github/workflows/` | CI and image publishing. |

`AGENTS.md` carries the machine-facing instructions for AI coding agents
working in this repo; humans can ignore it (or read it — it’s short).

## License

[MIT](LICENSE).
