# Metra

[Website](https://metra.sh) | [API Docs](https://metra.sh/docs)

Metra is a unified, real-time aggregation layer for cryptocurrency market data. It connects multiple upstream exchange WebSockets, normalizes the raw execution data into a standardized schema, and rebroadcasts it to downstream clients via Server-Sent Events (SSE).

The project abstracts away the complexity of managing multiple WebSocket connections, exchange-specific parsing, connection
age management, and heartbeat maintenance. It is designed to serve as a firehose for quantitative analysis, algorithmic trading bots, and data archival systems.

The firehose API is free, and relies on the generous public data streams offered by the exchanges.

## Features

- **Unified Schema:** Consumes the formats from Binance and Bitstamp and outputs a consistent JSON structure.
- **Zero Configuration:** No API keys or authentication required.
- **Traffic Shaping:** Built-in throttling to control downstream message rates (e.g., limiting high-frequency streams to 100ms updates).
- **Lightweight Explorer:** Includes a built-in web dashboard for inspecting live feeds, built with Go Templ, Mithril.js, and Tailwind CSS v4.

## API Documentation

The primary interface is a streaming REST endpoint over SSE.

### Endpoint

```http
GET /stream/{exchange}/{pair}/{event}
```

### Parameters

| Parameter | Type | Description |
| :--- | :--- | :--- |
| `exchange` | Path | Upstream source. Supported: `binance`, `bitstamp`. |
| `pair` | Path | Trading pair symbol (e.g., `btcusdt`, `ethusd`). |
| `event` | Path | Data type. Currently supported: `trade`. |
| `throttle` | Query | (Optional) Minimum interval between messages (e.g., `100ms`, `1s`, `2s`). |

### Example

```bash
# Stream Bitcoin trades from Binance, capped at one trade every 100ms
curl -N "https://metra.sh/stream/binance/btcusdt/trade?throttle=100ms"
```

## Running Locally

### Prerequisites

- Go 1.25+
- Tailwind CSS CLI (v4+)
- Templ

### Build & Run

```bash
# Generate templates
templ generate

# Build CSS
./tailwindcss -i web/static/css/input.css -o web/static/css/style.css --minify

# Compile and Run
go build .
./metra
```

The server will start on port `8080`.

### Docker

A multi-stage Dockerfile is provided for production deployments.

```bash
docker build -t metra .
docker run -p 8080:8080 metra
```

## Roadmap

The current implementation focuses on spot trade normalization. Future development will target:

1.  **Expanded Coverage:** Integration with Coinbase, Kraken, and Bybit.
2.  **Data Depth:** Support for Level 2 (Orderbook) and Ticker events.
3.  **Transport:** Optional gRPC/Protobuf streaming for high-throughput internal consumption. Maybe downstream WS too?
4.  **Analytics:** Maybe? Be nice to have some real-time sliding window aggregations (VWAP, OHLCV).

## Contributions

For anyone interested in contributing, please read [contribute.md](./contribute.md) first.
