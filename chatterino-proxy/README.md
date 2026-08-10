# Chatterino Proxy

A lightweight reverse proxy for Chatterino services, providing unified endpoints for Recent Messages and Link Resolver.

---

## Features

- Multiple upstreams per service  
- Automatic health monitoring with configurable endpoints  
- Latency tracking and fastest‑healthy selection  
- Parallel upstream racing (global or per‑service)  
- Custom upstream path identifiers for explicit selection

---

## Endpoints

| Endpoint         | Description                       |
|------------------|-----------------------------------|
| `/rm`            | Recent Messages API proxy         |
| `/link_resolver` | Link Resolver API proxy           |
| `/`              | Instance information              |
| `/health`        | Upstream health status            |

---

## Configuration

JSON configuration file (default: `config.json`).  
Override with env `PROXY_CONFIG=/path/to/config.json`.

### Minimal Example

```json
{
  "instance": {
    "maintainer": "YourName",
    "heartbeatInterval": 30,
    "timeout": 30,
    "healthTimeout": 5,
    "race": false
  },
  "rmInstances": {
    "race": true,
    "https://logs.zonian.dev/rm": {
      "path": "zonian",
      "health": "/health"
    },
    "https://recent-messages.robotty.de/api/v2/recent-messages": {
      "path": "rm",
      "health": "/"
    }
  },
  "link_resolverInstances": {
    "https://braize.pajlada.com/chatterino/link_resolver": {
      "path": "pajlada"
    }
  }
}
```

### Instance Settings

| Key                 | Default                  | Description                           |
|---------------------|--------------------------|---------------------------------------|
| `maintainer`        | `""`                     | Instance maintainer                   |
| `message`           | `""`                     | Custom message                        |
| `description`       | `""`                     | Instance description                  |
| `userAgent`         | `chatterino-proxy/1.0`   | User‑Agent sent to upstreams          |
| `heartbeatInterval` | `30`                     | Health‑check interval (seconds)       |
| `timeout`           | `30`                     | Upstream request timeout (seconds)    |
| `healthTimeout`     | `5`                      | Health‑check timeout (seconds)        |
| `race`              | `false`                  | Global race mode                      |

### Upstream Settings

Each upstream object can contain:

- `path` – identifier used to explicitly select this upstream (e.g., `"zonian"`)  
- `health` – custom health‑check endpoint (defaults to upstream URL)  
- `description` – human‑readable description  
- `maintainer` – upstream maintainer  
- Any extra fields are preserved as metadata (visible in `/health`)

---

## Upstream Selection

The proxy selects which upstream(s) to use based on the **query parameter** `chatterino-proxy-upstream`.

- **Explicit selection** – use one or more upstream identifiers (comma‑separated) in the URL:  
  `/rm/RyanPotat?chatterino-proxy-upstream=zonian` → forwards to `https://logs.zonian.dev/rm/RyanPotat`  
  `/rm/RyanPotat?chatterino-proxy-upstream=zonian,rm` → tries both (in race or sequential order)

- **All upstreams** – if the parameter is omitted or set to `all`, all healthy upstreams are considered.  
  With `race` disabled, the fastest healthy upstream is tried first; on failure, the next is attempted.  
  With `race` enabled, all are queried concurrently and the first successful (2xx) response wins.

- **Parameter stripping** – the `chatterino-proxy-upstream` parameter is **removed** before the request is forwarded to the upstream, so it does not interfere with the target API.

- **Fallback** – if explicitly selected upstreams are all unhealthy, the proxy automatically falls back to all healthy upstreams.

---

## Link Resolver

Works identically to `/rm`. Upstreams are configured under `link_resolverInstances`.  
Example request: `/link_resolver/...?chatterino-proxy-upstream=pajlada` selects the upstream with `path: "pajlada"`.

---

## Health Monitoring

- Health checks run every `heartbeatInterval` seconds.  
- If `health` is set, the proxy does a HEAD request to that endpoint (expects 200–399).  
- If not set, it checks the upstream URL itself (expects <500).  
- Unhealthy upstreams are excluded from routing.

The `/health` endpoint exposes detailed status, including latency, health, and metadata for each upstream.

---

## Examples

**Recent Messages – explicit upstream**  
`GET /rm/RyanPotat?chatterino-proxy-upstream=zonian`

**Recent Messages – all upstreams (race enabled)**  
`GET /rm/RyanPotat?chatterino-proxy-upstream=all` (or just omit the parameter)

**Recent Messages – multiple specific upstreams**  
`GET /rm/RyanPotat?chatterino-proxy-upstream=zonian,rm`

**Link Resolver – explicit**  
`GET /link_resolver/https://example.com?chatterino-proxy-upstream=pajlada`

**Health check**  
`GET /health`

---

## Running

### Build & Run

```bash
go mod tidy
go build -o chatterino-proxy .
./chatterino-proxy
```

The server listens on the port specified by the `PORT` environment variable (default: `8000`).

### Docker

```yaml
services:
  chatterino-proxy:
    build: .
    ports:
      - "8000:8000"
    volumes:
      - ./config.json:/app/config.json:ro
    environment:
      - PROXY_CONFIG=/config/config.json   # optional
      - PORT=8080                          # optional, default 8000
```

### Reverse Proxy (Caddy example)

```caddy
proxy.example.com {
    reverse_proxy chatterino-proxy:8000
}
```
