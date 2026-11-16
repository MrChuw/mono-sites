# File Server API

A personal-ish file hosting server.

The application provides authenticated uploads, automatic metadata removal, file deduplication through hard links, temporary file expiration, deletion tokens, API key management, and usage metrics.

Files that can't have a real preview (text, archives, binaries, ...) get a square byte-plot visualization of their raw bytes as a thumbnail instead — one byte per pixel, binvis-style colors (zeros black, text a green brightness gradient, high-entropy data rainbow). It is also the fallback when image/video preview generation fails.

Videos get a static JPEG frame thumbnail, an animated GIF preview (`/preview/p/`), and an animated WebP preview (`/preview/w/`) capped at ~3.9 MB. Animated image uploads (GIF/WebP files) also get an animated WebP preview, so link resolvers (e.g. the Chatterino link resolver, which receives the preview through `og:image`) can show them moving.

Previews are generated asynchronously after the upload response is already out, so preview links are always sent up front. A request for a preview that doesn't exist yet just holds the connection open until the asset lands (it 404s if it never shows up).

## Endpoints

| Endpoint                  | Description                             |
|---------------------------|-----------------------------------------|
| `POST /api/upload`        | Upload a file with metadata cleaning    |
| `POST /api/uploaddoxx`    | Upload a file without metadata cleaning |
| `GET /api/delete/{token}` | Delete a file using its deletion token  |
| `POST /api/keys`          | Create a new API key (OWNER only)       |
| `GET /api/metrics/user`   | User metrics                            |
| `GET /api/metrics/admin`  | Administrator metrics                   |
| `GET /{path}`             | Download a file                         |
| `GET/POST /status`        | Owner stats dashboard (OWNER key form, `?format=json` export) |
| `POST /api/sharex/config` | Easy way to create sharex config        |
| `/`                       | Status page                             |

## Upload Options

Uploads support:

- Tags
- Upload scopes through the `X-Name` header
- Optional `ttl_seconds`
- Automatic role and owner tags

## Running

### Docker (Recommended)

Prebuilt container images are published through GitHub.

```bash
docker run -d \
  --name file-server \
  -p 8000:8000 \
  -v ./db:/app/db \
  -v ./uploads:/app/uploads \
  --env-file .env \
  ghcr.io/mrchuw/uploadserver:latest
```

Or with Docker Compose:

```yaml
services:
  file-server:
    image: ghcr.io/mrchuw/uploadserver:latest
    container_name: file-server
    restart: unless-stopped

    ports:
      - "8000:8000"

    env_file:
      - .env

    volumes:
        - ${PWD}/uploads:/app/uploads
        - ./db:/app/db
```

### Build from source

```bash
go build -o file-server .
```

Run the server:

```bash
./file-server
```

## CLI

Create the first API key:

```bash
./file-server create-key --owner admin --role owner
```

Create another API key:

```bash
./file-server create-key --owner alice --role vip --max-size-mb 1024
```

Available roles:

- `owner`
- `vip`
- `normal`

## Reverse Proxy

This application is designed to run behind a reverse proxy. The recommended setup uses Caddy with `file_server` and internal redirects for efficient static file delivery, reducing the load on the application.

It can also be deployed behind other reverse proxies such as Nginx, or run directly without a reverse proxy if preferred.

Example Caddy configuration:

```text
upload.domain {
    encode gzip zstd

    header {
        X-Content-Type-Options "nosniff"
        X-Frame-Options "DENY"
        Referrer-Policy "strict-origin-when-cross-origin"
    }

    handle /api* {
        reverse_proxy upload_server:8000
    }

    handle {
        reverse_proxy upload_server:8000 {
            header_up X-Handled-By "Caddy"

            @accel {
                header X-Accel-Redirect *
            }

            handle_response @accel {
                vars back_to_caddy {rp.header.X-Accel-Redirect}
                rewrite * {vars.back_to_caddy}
                uri strip_prefix /internal-media
                root * /uploads
                file_server {
                    hide .*
                }
            }
        }
    }
}
```

## Service Stats

Beyond the API metrics above, the service keeps its own anonymous usage statistics. The homepage (`/`) stays minimal — uptime, serving since, storage used, transferred — but there is a full dashboard behind `/status` for the owner.

To open it, paste your **OWNER API key** into the form field; the key is kept in an `HttpOnly` cookie scoped to `/status` for 8 hours so refreshes don't ask again. Machine reads can send the `X-API-Key` header (OWNER key) and request `GET /status?format=json` for the same data as JSON — raw byte counts, snake_case keys.

Both pages auto-update: the dashboard reloads itself every 30 seconds, the homepage every 60 (refreshes carry `?auto=1` and are excluded from Umami, so they don't pollute analytics).

The dashboard shows:

- Live state — uptime, storage, preview queue depth and average preview duration
- All-time counters — transferred (files + previews), received pre-clean vs stored post-clean, files uploaded/deleted, bytes freed by trash purges, dedup hits and bytes saved, resolver page views, auth failures, db busy errors, preview failures
- Last 30 days — bar charts for served / received / uploads per UTC day, plus tables for events by kind, content classes (video/image/audio/archive/doc/other, and jpg/gif/webp/byteplot for previews), uploads per key owner, and the top served files

How it's stored: counters accumulate in memory and flush every minute to SQLite as raw additive deltas (`status_deltas`, prunable) plus hourly/daily rollups (`status_hourly`, `status_daily`, never pruned, UTC buckets) and a single all-time row (`service_stats`). Raw delta retention is off by default; set `STATUS_RAW_RETENTION_DAYS` to keep only the last N days of raw rows (old rows are pruned at startup and hourly once enabled) — rollups keep forever.

On the first start after this update, a one-time backfill fills the stats system with everything the pre-existing tables can still tell us: files uploaded/deleted, stored and freed bytes, dedup hits and savings from the surviving hardlinks, and real per-day upload/delete history for the 30-day charts. It runs once (guarded by a flag in `service_stats`) in a single transaction, so a crash just retries on the next start. Three honest approximations, because the raw data no longer exists: historical "received" bytes equal the stored size (pre-clean wire sizes were never recorded), deleted files' uploads land on key 0 (delete logs don't carry the key) and dedup is counted from active files only, and all historical buckets use the "other" content class (MIME wasn't recorded back then). What can't be reconstructed starts at zero: resolver hits, auth failures, busy errors, preview failures, per-file request counts, and served bytes per day (transferred only ever existed as one all-time number).

Delivery events record no owners, IPs or paths; upload events carry only the API key id. Range requests count the requested byte span instead of the whole file, and 302 redirects to the CDN host count the redirect, not the payload.

## Analytics

Umami events are generated for:

- Uploads
- Downloads
- Deletions
- API key creation
- Page views

## Metrics

The API provides both user and administrator metrics, including:

- Upload count
- Deletion count
- Storage usage
- Disk information
- Upload history
- Deduplication statistics
- Server uptime
- Owner rankings

## Roles

| Role | Description |
|------|-------------|
| `OWNER` | Full administrative access |
| `VIP` | Double upload size limit |
| `NORMAL` | Standard user |

## Metadata Cleaning

The standard upload endpoint removes metadata from supported image and media formats before saving the file.

The `/api/uploaddoxx` endpoint stores files without modifying metadata.

## Background Tasks

A background task runs every 60 seconds to:

- Delete expired uploads
- Permanently purge files waiting in the trash
