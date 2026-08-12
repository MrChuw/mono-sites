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
| `userAgent`         | `chatterino-proxy/1.1.3` | User‑Agent sent to upstreams          |
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
- `unsupported` – list of keywords excluding this upstream from matching requests (exact username match on `/rm`, substring hostname match on `/link_resolver`) — see [Unsupported Identifiers](#unsupported-identifiers)  
- `replace` – per‑domain path rewrite rules applied before forwarding. Link Resolver only — see [Dynamic URL Replace](#dynamic-url-replace-link-resolver-only)  
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

## Bypassing Specific Upstreams

The query parameter `chatterino-proxy-bypass` excludes one or more specific upstreams for a given identifier — a username on `/rm`, or a hostname on `/link_resolver`.

- **Format** – `identifier,upstreamPath` (comma‑separated pairs). Multiple pairs can be chained in one value: `identifier1,upstream1,identifier2,upstream2`.
- **Multiple parameters** – any query parameter whose name starts with `chatterino-proxy-bypass` (e.g. `chatterino-proxy-bypass2`) is also parsed and merged, so you can split bypass rules across several parameters if needed.
- The bypass only applies when the identifier matches the current request (username for `/rm`, hostname for `/link_resolver`). Matching is case‑insensitive.
- **Fallback** – if the explicitly requested upstream(s) (`chatterino-proxy-upstream`) end up being bypassed, the proxy automatically falls back to all other configured upstreams (still respecting `unsupported` and the bypass rule itself).
- If bypassing removes every candidate upstream, even after the fallback, the proxy responds with `400 Bad Request`.

**Examples**

`GET /rm/RyanPotat?chatterino-proxy-bypass=RyanPotat,zonian`
→ skips the `zonian` upstream for user `RyanPotat`.

`GET /rm/RyanPotat?chatterino-proxy-upstream=zonian&chatterino-proxy-bypass=RyanPotat,zonian`
→ `zonian` was both explicitly requested and bypassed, so the proxy falls back to all other upstreams.

`GET /link_resolver/https://www.instagram.com/p/XYZ?chatterino-proxy-bypass=instagram.com,mrchuw`
→ skips the `mrchuw` upstream for `instagram.com` links.

---

## Dynamic URL Replace (Link Resolver only)

The query parameter `chatterino-proxy-replace` rewrites part of the target URL's path for a specific domain before the request is forwarded.

- **Format** – `domain,find,replace` (comma‑separated triples). Multiple triples can be chained in one value: `domain1,find1,replace1,domain2,find2,replace2`.
- **Multiple parameters** – any query parameter whose name starts with `chatterino-proxy-replace` (e.g. `chatterino-proxy-replace2`) is also parsed and merged.
- Rules apply to the domain that best matches the target URL's host.
- If an upstream also defines a static `replace` rule (in the config) for the same domain and `find` pattern, the value supplied via `chatterino-proxy-replace` takes precedence.

**Example**

`GET /link_resolver/https%3A%2F%2Fwww.instagram.com%2Fp%2FXYZ?chatterino-proxy-replace=instagram.com,/p/,/reel/`
→ the path is rewritten to `/reel/XYZ` before forwarding.

---

## Unsupported Identifiers

Upstreams can declare an `unsupported` list of keywords in the config, excluding them from requests that match a given identifier — a username on `/rm` (exact match), or a hostname on `/link_resolver` (matched as a case‑insensitive substring, e.g. `"unsupported": ["instagram", "facebook"]` matches `www.instagram.com`).

- Any upstream whose `unsupported` list matches the identifier is skipped for that request.
- If an explicitly requested upstream (`chatterino-proxy-upstream`) turns out to be unsupported for that identifier, the proxy automatically falls back to all other configured upstreams that do support it.
- If every configured upstream is unsupported for the identifier, the proxy responds with `400 Bad Request`.

---

## Fallback on Failure (Link Resolver only)

By default, when `chatterino-proxy-upstream` explicitly selects one or more upstreams, the proxy only tries those — if they fail, it gives up. The query parameter `chatterino-proxy-fallback=true` changes that: if the explicitly requested upstream(s) fail, the proxy automatically races all other configured upstreams (still respecting `unsupported`, `bypass`, and `replace`) and returns the first one that succeeds.

- **Format** – `chatterino-proxy-fallback=true` (also accepts `1` or `yes`, case‑insensitive). Any other value, or omitting the parameter, keeps the default behavior (only the requested upstream is tried).
- A failure includes both a failed request (network error, non‑2xx/3xx HTTP status) and an upstream that responds with HTTP 200 but embeds an error status in its JSON body (some upstreams do this).
- The fallback round always races the remaining upstreams concurrently, regardless of the service's `race` setting.
- This only applies when `chatterino-proxy-upstream` was used to explicitly select upstream(s). It has no effect when no upstream is specified (or `all` is used), since all upstreams are already tried in that case.

**Example**

`GET /link_resolver/https://example.com?chatterino-proxy-upstream=mrchuw&chatterino-proxy-fallback=true`
→ if `mrchuw` fails, the proxy races the other configured upstreams and returns the first successful response.

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

**Link Resolver – bypass an upstream for a domain**  
`GET /link_resolver/https://www.instagram.com/p/XYZ?chatterino-proxy-bypass=instagram.com,mrchuw`

**Link Resolver – dynamic path replace**  
`GET /link_resolver/https%3A%2F%2Fwww.instagram.com%2Fp%2FXYZ?chatterino-proxy-replace=instagram.com,/p/,/reel/`

**Link Resolver – fallback to other upstreams on failure**  
`GET /link_resolver/https://example.com?chatterino-proxy-upstream=mrchuw&chatterino-proxy-fallback=true`

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
      - ./config.json:/config.json:ro
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
