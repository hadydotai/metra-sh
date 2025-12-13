import { sleep } from "k6";
import exec from "k6/execution";
import { Counter, Rate, Trend } from "k6/metrics";

// NOTE:
// This test opens Server-Sent Events (SSE) streams.
// It requires the SSE extension: https://github.com/phymbert/xk6-sse
//
// If `k6 run` errors with "unknown module: k6/x/sse", build k6 with xk6:
//   go install go.k6.io/xk6/cmd/xk6@latest
//   xk6 build --with github.com/phymbert/xk6-sse@latest
//   ./k6 run loadtest/metra_stream_sse.k6.js
import sse from "k6/x/sse";

const streamOpens = new Counter("metra_stream_opens");
const streamEvents = new Counter("metra_stream_events");
const streamCloseOK = new Counter("metra_stream_close_ok");
const streamErrors = new Counter("metra_stream_errors");

const openLatencyMs = new Trend("metra_stream_open_latency_ms", true);
const holdMs = new Trend("metra_stream_hold_ms", true);

const openOKRate = new Rate("metra_stream_open_ok_rate");
const statusOKRate = new Rate("metra_stream_status_ok_rate");

function parseEnvInt(name, fallback) {
  const raw = __ENV[name];
  if (!raw) return fallback;
  const val = Number.parseInt(raw, 10);
  return Number.isFinite(val) ? val : fallback;
}

function parseEnvFloat(name, fallback) {
  const raw = __ENV[name];
  if (!raw) return fallback;
  const val = Number.parseFloat(raw);
  return Number.isFinite(val) ? val : fallback;
}

function parseHoldSeconds(raw) {
  // Minimal duration parser for env var HOLD_FOR (supports ms/s/m/h).
  // Examples: "30s", "2m", "500ms".
  if (!raw) return 30;
  const m = String(raw).trim().match(/^(\d+(?:\.\d+)?)(ms|s|m|h)$/);
  if (!m) return 30;
  const n = Number.parseFloat(m[1]);
  const unit = m[2];
  if (!Number.isFinite(n) || n <= 0) return 30;
  if (unit === "ms") return Math.max(0.001, n / 1000);
  if (unit === "s") return n;
  if (unit === "m") return n * 60;
  if (unit === "h") return n * 3600;
  return 30;
}

function throttleLabel(throttle) {
  if (!throttle) return "realtime";
  return throttle;
}

function buildStreamURL(baseURL, exchange, pair, throttle) {
  const root = baseURL.replace(/\/+$/, "");
  const path = `/stream/${exchange}/${pair}/trade`;
  if (!throttle) return `${root}${path}`;
  return `${root}${path}?throttle=${encodeURIComponent(throttle)}`;
}

const BASE_URL = __ENV.BASE_URL || "https://metra.sh";
const HOLD_FOR_SECONDS = parseHoldSeconds(__ENV.HOLD_FOR || "30s");

// These are the endpoints you asked to test.
// You can override pairs with BINANCE_PAIR / BITSTAMP_PAIR if needed.
const BINANCE_PAIR = (__ENV.BINANCE_PAIR || "btcusdt").toLowerCase();
const BITSTAMP_PAIR = (__ENV.BITSTAMP_PAIR || "btcusd").toLowerCase();

const ENDPOINTS = [
  { exchange: "binance", pair: BINANCE_PAIR, throttle: null },
  { exchange: "binance", pair: BINANCE_PAIR, throttle: "100ms" },
  { exchange: "binance", pair: BINANCE_PAIR, throttle: "1s" },
  { exchange: "bitstamp", pair: BITSTAMP_PAIR, throttle: null },
  { exchange: "bitstamp", pair: BITSTAMP_PAIR, throttle: "100ms" },
  { exchange: "bitstamp", pair: BITSTAMP_PAIR, throttle: "1s" },
];

function pickEndpoint() {
  // Distribute evenly across endpoints.
  const i = exec.scenario.iterationInTest % ENDPOINTS.length;
  return ENDPOINTS[i];
}

function tagsFor(ep) {
  return {
    exchange: ep.exchange,
    pair: ep.pair,
    throttle: throttleLabel(ep.throttle),
  };
}

function buildStages() {
  // Default is a ramp-to-failure style profile; tune via env vars.
  //
  // Env vars:
  // - STAGES: a literal k6 JSON stages array (advanced)
  // - RAMP_EVERY: duration string (default "30s")
  // - RAMP_STEP: vus to add per stage (default 50)
  // - RAMP_STEPS: number of ramp stages (default 10)
  // - HOLD_LAST: final steady-state hold duration (default "60s")
  const rawStages = __ENV.STAGES;
  if (rawStages) {
    // Example:
    // STAGES='[{"duration":"30s","target":100},{"duration":"30s","target":200}]'
    try {
      const parsed = JSON.parse(rawStages);
      if (Array.isArray(parsed) && parsed.length > 0) return parsed;
    } catch (_) {
      // Fall through to generated stages.
    }
  }

  const every = __ENV.RAMP_EVERY || "30s";
  const step = parseEnvInt("RAMP_STEP", 50);
  const steps = parseEnvInt("RAMP_STEPS", 10);
  const holdLast = __ENV.HOLD_LAST || "60s";

  const stages = [];
  for (let i = 1; i <= steps; i += 1) {
    stages.push({ duration: every, target: step * i });
  }
  stages.push({ duration: holdLast, target: step * steps });
  return stages;
}

export const options = {
  scenarios: {
    sse_streams: {
      executor: "ramping-vus",
      startVUs: parseEnvInt("START_VUS", 0),
      stages: buildStages(),
      gracefulRampDown: __ENV.GRACEFUL_RAMP_DOWN || "10s",
      gracefulStop: __ENV.GRACEFUL_STOP || "30s",
      exec: "stream",
    },
  },
  // Abort early when error rate crosses the threshold (useful for “test to failure”).
  thresholds: {
    metra_stream_open_ok_rate: [
      {
        threshold: `rate>${parseEnvFloat("MIN_OPEN_OK_RATE", 0.98)}`,
        abortOnFail: true,
        delayAbortEval: __ENV.ABORT_DELAY || "30s",
      },
    ],
    metra_stream_status_ok_rate: [
      {
        threshold: `rate>${parseEnvFloat("MIN_STATUS_OK_RATE", 0.98)}`,
        abortOnFail: true,
        delayAbortEval: __ENV.ABORT_DELAY || "30s",
      },
    ],
  },
  // If you're hitting OS limits before server limits, you may need to raise ulimit on the load gen machine.
  // See loadtest/README.md for tips.
};

export function stream() {
  const ep = pickEndpoint();
  const url = buildStreamURL(BASE_URL, ep.exchange, ep.pair, ep.throttle);
  const tags = tagsFor(ep);

  streamOpens.add(1, tags);

  const startedAt = Date.now();
  let openedAt = 0;
  let opened = false;

  let resp;
  try {
    resp = sse.open(
      url,
      {
        headers: { Accept: "text/event-stream" },
        tags,
        // k6 timeout for the initial HTTP request + handshake.
        // The connection will remain open until we close it in the open handler.
        timeout: __ENV.CONNECT_TIMEOUT || "15s",
      },
      function (client) {
        client.on("open", function () {
          opened = true;
          openedAt = Date.now();
          openLatencyMs.add(openedAt - startedAt, tags);
          openOKRate.add(true, tags);

          // Hold the connection open to simulate a connected SSE client.
          sleep(HOLD_FOR_SECONDS);
          holdMs.add(Date.now() - openedAt, tags);

          try {
            client.close();
            streamCloseOK.add(1, tags);
          } catch (_) {
            // Ignore close errors; status/ok rates capture failures.
          }
        });

        client.on("event", function (_event) {
          // We don't parse payloads here; this test is about concurrent connections.
          // Keeping the handler prevents the SSE client from being “write only”.
          streamEvents.add(1, tags);
        });

        client.on("error", function (_err) {
          streamErrors.add(1, tags);
          openOKRate.add(false, tags);
          try {
            client.close();
          } catch (_) {}
        });
      }
    );
  } catch (_) {
    streamErrors.add(1, tags);
    openOKRate.add(false, tags);
    statusOKRate.add(false, tags);
    return;
  }

  // If the connection never opened, record it as a failed open attempt.
  if (!opened) {
    openOKRate.add(false, tags);
  }

  // sse.open returns an HTTPResponse (or similar) when the connection closes.
  // For this endpoint, success should be HTTP 200.
  const statusOK = resp && resp.status === 200;
  statusOKRate.add(Boolean(statusOK), tags);
  if (!statusOK) {
    streamErrors.add(1, tags);
  }
}

// Allow `k6 run --vus ... --duration ...` style overrides by providing a default entrypoint.
export default function () {
  return stream();
}
