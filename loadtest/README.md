# Load testing (k6)

This directory contains k6 scripts for load testing Metra’s SSE streaming API (`/stream/...`).

## Script: `loadtest/metra_stream_sse.k6.js`

Opens Server-Sent Events (SSE) streams and holds the connection open (simulates concurrent connected users).

### Endpoints exercised

- `GET /stream/binance/btcusdt/trade` (realtime / `100ms` / `1s`)
- `GET /stream/bitstamp/btcusd/trade` (realtime / `100ms` / `1s`)

### Prerequisite: SSE support in k6

This script uses the SSE extension: `github.com/phymbert/xk6-sse`.

If your k6 supports automatic extension provisioning, `k6 run` will fetch/build a compatible binary automatically.
Otherwise, build a custom k6:

```bash
go install go.k6.io/xk6/cmd/xk6@latest
xk6 build --with github.com/phymbert/xk6-sse@latest
./k6 version
```

### Basic run (against production)

```bash
k6 run loadtest/metra_stream_sse.k6.js
```

### Tune ramp profile (test-to-failure)

By default the script ramps VUs up in steps and aborts when the open/status OK rate drops below the threshold.

Example: ramp by 200 VUs every 30s, for 20 steps, hold 2 minutes at the top:

```bash
RAMP_STEP=200 RAMP_STEPS=20 HOLD_LAST=2m ./k6 run loadtest/metra_stream_sse.k6.js
```

### Control connection hold time

Each VU opens one SSE connection per iteration and keeps it open for `HOLD_FOR`:

```bash
HOLD_FOR=60s ./k6 run loadtest/metra_stream_sse.k6.js
```

### Quick sanity run (CLI overrides)

This uses k6 CLI flags (which override the script’s `scenarios`), so the script exports a `default` function too:

```bash
HOLD_FOR=1s k6 run --vus 10 --duration 30s loadtest/metra_stream_sse.k6.js
```

### Override base URL / pairs

```bash
BASE_URL=https://metra.sh BINANCE_PAIR=btcusdt BITSTAMP_PAIR=btcusd ./k6 run loadtest/metra_stream_sse.k6.js
```

### Common “load generator” limits (before server failure)

If you hit client-side errors first (e.g. `too many open files`), raise limits on the machine running k6:

```bash
ulimit -n 100000
```
