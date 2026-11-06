# deepseek-search-fetch-mcp

[中文](README.md) | English

Exposes DeepSeek's official web search and fetch capabilities as an MCP server, plus a directly invocable CLI. The project does not implement retrieval logic itself; it handles the MCP protocol, tool descriptions, parameter validation, credential injection, and local caching.

- **MCP tools**: `web_ext_search`, `web_ext_fetch`
- **CLI subcommands**: `server`, `search`, `fetch`, `flushall`
- **Cache**: SQLite; search prewarms it and fetch reuses it, with TTL, capacity eviction, and one-shot flush
- **Transport**: Streamable HTTP, stateless, JSON responses
- **Default listener**: `127.0.0.1:18081`

> Get an API Key from the DeepSeek platform before use.

---

## 1. Install and Run

### Requirements

- Go 1.25 or newer
- No CGO needed: SQLite uses the pure-Go driver `modernc.org/sqlite`
- In mainland China, set the Go proxy first:

```bash
export GOPROXY=https://goproxy.cn,direct
```

### Build from Go Source (local use)

```bash
git clone https://github.com/bob56621517/deepseek-search-fetch-mcp.git
cd deepseek-search-fetch-mcp

# Linux / macOS
go build -o deepseek-search-fetch-mcp ./cmd/deepseek-search-fetch-mcp

# Windows
go build -o deepseek-search-fetch-mcp.exe ./cmd/deepseek-search-fetch-mcp
```

The binary is created in the current directory; run it directly:

```bash
./deepseek-search-fetch-mcp server        # Linux / macOS
.\deepseek-search-fetch-mcp.exe server    # Windows
```

To set a version, add `-ldflags "-X main.version=v1.2.3"` (otherwise it is `dev+local`).

### Install with go install (download and install in one command)

`go install` downloads the source from the module proxy, compiles it, and installs the binary into `$(go env GOPATH)/bin`:

```bash
go install github.com/bob56621517/deepseek-search-fetch-mcp/cmd/deepseek-search-fetch-mcp@latest
```

- `@latest` picks the newest version; use `@v1.2.3` for a specific version or `@main` for the main branch
- The binary is placed in `$(go env GOPATH)/bin` (on Windows usually `%USERPROFILE%\go\bin`)
- Add that directory to `PATH` to run it directly:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"   # Linux / macOS
deepseek-search-fetch-mcp server
```

```powershell
$env:Path += ";$(go env GOPATH)\bin"       # Windows PowerShell
deepseek-search-fetch-mcp.exe server
```

### Start the MCP server

```bash
deepseek-search-fetch-mcp server --port 18081
```

**The `server` subcommand must be given explicitly; running without any arguments prints usage and exits with an error.**

### Docker

```bash
docker build -t deepseek-search-fetch-mcp .

docker run -d --name deepseek-search-fetch-mcp \
  -p 18081:18081 \
  -e DEEPSEEK_API_KEY=sk-... \
  -v deepseek-sf-data:/data \
  deepseek-search-fetch-mcp
```

- Inside the container `HOME=/data`, so config, logs, cache, and access keys live under `/data/.deepseek_search_fetch`.
- Without a named volume Docker uses an anonymous one; data is hard to reuse after the container is recreated, so always mount `/data`.
- The container runs `server` by default; to run CLI commands inside it, override the command:

```bash
docker run --rm -e DEEPSEEK_API_KEY=sk-... \
  -v deepseek-sf-data:/data \
  deepseek-search-fetch-mcp search "DeepSeek V4"
```

Manage access keys in the container data directory (no DeepSeek API Key needed):

```bash
docker run --rm -v deepseek-sf-data:/data \
  deepseek-search-fetch-mcp apikey add laptop
```

The image is also published to GHCR: `ghcr.io/bob56621517/deepseek-search-fetch-mcp`.

### Windows service

To run as an auto-start Windows service, see [Windows service deployment](docs/deployment/windows-service.md).

### Install paths

| Content | Path |
|---|---|
| Config | `~/.deepseek_search_fetch/config.json` |
| Logs | `~/.deepseek_search_fetch/logs/server.log` |
| Cache | `~/.deepseek_search_fetch/cache.db` (`-wal` and `-shm` may appear alongside) |
| Access key store | `~/.deepseek_search_fetch/auth.db` (hashes and masked fragments only; `-wal` and `-shm` may appear alongside) |

On Windows `~` maps to your user directory, e.g. `C:\Users\<name>`.

The data directory defaults to `~/.deepseek_search_fetch` and can be overridden with `--data-dir` or the `DEEPSEEK_SEARCH_FETCH_HOME` environment variable (precedence: startup flag > environment variable > default). Config, cache, access keys, and logs always live in the same data directory.

- On first start the directory and a config template are created automatically; the Key is empty.
- Config is read once at process start; restart the service after changing it. CLI commands re-read it on every run. Access keys are managed by `apikey` and take effect immediately without a restart.
- If the user home directory cannot be determined and no data directory is set, the program skips config file creation and uses only environment variables and startup flags; caching is unavailable and the access-key store cannot be opened, so `server` fails to start. When running under a non-interactive account such as a service or container, always set the data directory explicitly or make sure the home directory points to the user directory (see [Windows service deployment](docs/deployment/windows-service.md)).

### Logging

- Server log file: `~/.deepseek_search_fetch/logs/server.log`
- Authentication/rate-limit rejections are also written there with `event`, `remote`, `status`, `reason` fields for fail2ban; no keys are logged
- Rotation: 10 MiB per file, keeping up to 5 files (current `server.log` + 4 backups), backups named `server-<timestamp>.log`
- `server` mode: logs go to both stderr and the file
- CLI mode: logs go **only to the file**; stdout carries JSON only
- If the user home directory cannot be determined: `server` mode logs to stderr only, CLI mode discards logs

---

## 2. Configuration

The config file defaults to `~/.deepseek_search_fetch/config.json`; the data directory can be overridden with `--data-dir` or `DEEPSEEK_SEARCH_FETCH_HOME`:

```json
{
  "deepseek_api_key": "sk-...",
  "deepseek_api_url": "https://api.deepseek.com",
  "server_host": "127.0.0.1",
  "server_port": 18081,
  "request_timeout": "30s",
  "fetch_timeout": "60s",
  "cache_ttl": "30m",
  "cache_max_bytes": 1073741824,
  "default_lang": "",
  "rate_limit_rps": 10,
  "rate_limit_burst": 20,
  "auth_fail_limit": 10,
  "auth_fail_window": "1m",
  "auth_fail_block": "5m",
  "ip_rate_limit_rps": 0,
  "ip_rate_limit_burst": 0,
  "trusted_proxies": [],
  "http_max_idle_conns": 5,
  "http_max_idle_conns_per_host": 5,
  "http_idle_conn_timeout": "90s",
  "http_tls_handshake_timeout": "10s",
  "reset_connection_on_error": true
}
```

### Precedence

**Command-line flags > environment variables > config file > defaults**

### Settings

| Setting | Default | Description |
|---|---|---|
| `deepseek_api_key` | empty | DeepSeek API Key; required by `server`, `search`, `fetch`; not needed by `flushall` or `apikey` |
| `deepseek_api_url` | `https://api.deepseek.com` | Upstream base URL |
| `server_host` | `127.0.0.1` | Listener address in `server` mode |
| `server_port` | `18081` | Listener port in `server` mode |
| `request_timeout` | `30s` | Timeout for regular upstream requests such as search |
| `fetch_timeout` | `60s` | Timeout for fetch requests; browser rendering is slower, so it is configured separately |
| `cache_ttl` | `30m` | Cache entry lifetime; content and summary are evaluated separately |
| `cache_max_bytes` | `1073741824` (1 GiB) | Page-level live-size limit, minimum 1 MiB; not a disk-usage limit |
| `default_lang` | empty | Default language completion: empty disables it, `auto` reads `LC_ALL`/`LC_MESSAGES`/`LANG`, or set a two-letter code such as `zh`; `zh-CN` is rejected |
| `rate_limit_rps` | `10` | Token-bucket rate per access key; `0` disables per-key rate limiting |
| `rate_limit_burst` | `20` | Burst capacity per access key; must be at least 1 when rate limiting is enabled |
| `auth_fail_limit` | `10` | Authentication failures allowed per IP within the window; `0` disables failure blocking |
| `auth_fail_window` | `1m` | Window for counting authentication failures |
| `auth_fail_block` | `5m` | Block duration after the failure threshold is reached |
| `ip_rate_limit_rps` | `0` | All-request rate per client IP; `0` disables it |
| `ip_rate_limit_burst` | `0` | Burst capacity per client IP; must be at least 1 when IP rate limiting is enabled |
| `trusted_proxies` | `[]` | Trusted proxy CIDR list; include only the reverse proxy addresses, never client subnets |
| `http_max_idle_conns` | `5` | Maximum idle connections overall |
| `http_max_idle_conns_per_host` | `5` | Maximum idle connections per host |
| `http_idle_conn_timeout` | `90s` | Idle connection timeout |
| `http_tls_handshake_timeout` | `10s` | TLS handshake timeout |
| `reset_connection_on_error` | `true` | Drop idle connections after any upstream failure so the next call reconnects |

> Authentication and rate-limit settings (`rate_limit_*`, `auth_fail_*`, `ip_rate_limit_*`, `trusted_proxies`) are read from the config file only and apply to `server` mode. There are no environment variables or command-line flags for them. Access keys themselves are not stored in the config file; manage them with the `apikey` commands.

### Environment variables

| Variable | Setting |
|---|---|
| `DEEPSEEK_API_KEY` | `deepseek_api_key` |
| `DEEPSEEK_API_URL` | `deepseek_api_url` |
| `DEEPSEEK_SEARCH_FETCH_HOME` | Data directory (default `~/.deepseek_search_fetch`) |
| `DEEPSEEK_SEARCH_FETCH_HOST` | `server_host` |
| `DEEPSEEK_SEARCH_FETCH_PORT` | `server_port` |
| `DEEPSEEK_DEFAULT_LANG` | `default_lang` |

### Command-line flags

Global flags are available on every subcommand but must be placed **after** the subcommand:

```bash
deepseek-search-fetch-mcp server --deepseek-api-key sk-... --port 18081
deepseek-search-fetch-mcp search --deepseek-api-key sk-... "query"
```

| Flag | Applies to | Description |
|---|---|---|
| `--deepseek-api-key` | all | Override the API Key |
| `--deepseek-api-url` | all | Override the base URL |
| `--data-dir` | all | Data directory, overriding the default `~/.deepseek_search_fetch` |
| `--host` | `server` | Listener address |
| `--port` | `server` | Listener port |
| `--count` | `search` | Number of results, default 10 |
| `--content` | `search` | Also return page content |
| `--summary` | `fetch` | Prefer the cached summary |
| `--refresh` | `fetch` | Skip the cache and force a refresh |

> Go's `flag` parser stops at the first positional argument, so options must precede `<query>` / `<url>`.

---

## 3. CLI Usage

```bash
deepseek-search-fetch-mcp server   [--host 127.0.0.1] [--port 18081]
deepseek-search-fetch-mcp search   [--count 10] [--content] <query>
deepseek-search-fetch-mcp fetch    [--summary] [--refresh] <url>
deepseek-search-fetch-mcp flushall
deepseek-search-fetch-mcp apikey   add <name>
deepseek-search-fetch-mcp apikey   list
deepseek-search-fetch-mcp apikey   remove <name>
deepseek-search-fetch-mcp --help
```

### `server`

Starts the MCP server. Defaults to `127.0.0.1:18081`.

```bash
deepseek-search-fetch-mcp server
deepseek-search-fetch-mcp server --host 0.0.0.0 --port 18081
```

### `search`

Calls upstream search directly and prints JSON.

- Returns `summary` by default, not content
- `--content` also returns `content`
- `--count` defaults to 10; 0 or omitted means 10; 1–4 is treated as 5; negative values or anything above 50 is an error
- Results are written to the cache; **search itself never uses cached results** (it always queries upstream)
- Options go before the query

```bash
deepseek-search-fetch-mcp search --count 10 "DeepSeek V4 release"
deepseek-search-fetch-mcp search --count 5 --content "Go 1.25 release notes"
```

Example output (with `--content`, each result also carries `content`):

```json
{
  "results": [
    {
      "title": "Example title",
      "url": "https://example.com/article",
      "snippet": "A short excerpt relevant to the query",
      "summary": "A condensed version of the page, usually 1–6k characters",
      "site_name": "Example",
      "date": "2026-10-23T08:00:00Z",
      "rerank_score": 2.5,
      "authority": 3
    }
  ]
}
```

### `fetch`

Calls the fetch flow directly and prints JSON.

- Cache first; upstream is queried only on a miss
- **Content mode is never paginated** and always returns the full text
- `--summary`: prefer the summary cached by search; fall back to full content when no summary exists
- `--refresh`: skip the cache read, refetch from upstream, and discard the old summary for that URL

```bash
# Full content
deepseek-search-fetch-mcp fetch "https://example.com/article"

# Prefer the cached summary
deepseek-search-fetch-mcp fetch --summary "https://example.com/article"

# Force refresh
deepseek-search-fetch-mcp fetch --refresh "https://example.com/article"
```

Example full content output:

```json
{
  "kind": "content",
  "url": "https://example.com/article",
  "title": "Example title",
  "content": "Full Markdown content...",
  "contentLength": 14407,
  "offset": 0,
  "length": 14407,
  "hasMore": false,
  "hasSummary": true,
  "source": "cache",
  "cachedAt": "2026-10-23T08:00:00Z"
}
```

Example summary hit:

```json
{
  "kind": "summary",
  "url": "https://example.com/article",
  "title": "Example title",
  "summary": "A condensed version of the page...",
  "contentLength": 2421,
  "offset": 0,
  "length": 1675,
  "hasSummary": true,
  "summaryQuery": "DeepSeek V4 release",
  "source": "cache",
  "cachedAt": "2026-10-23T08:00:00Z"
}
```

When the summary is missing it falls back to content:

```json
{
  "kind": "content",
  "hasSummary": false,
  "hint": "No cached summary is available for this URL; content was returned instead. Call web_ext_search first if you need a summary."
}
```

Field reference:

| Field | Description |
|---|---|
| `kind` | `content` or `summary` |
| `contentLength` | Total content length in runes |
| `offset` | Start offset; always 0 for summaries and for full CLI content |
| `length` | Number of characters returned this time |
| `hasMore` | Content mode: whether another page remains; always `false` for full CLI content; absent in summary mode |
| `nextOffset` | Offset of the next page; omitted when finished; only present in MCP pagination mode |
| `hasSummary` | Whether a cached summary is available |
| `source` | `cache` (cache hit) or `upstream` (live fetch) |
| `cachedAt` | Cache timestamp, UTC RFC 3339 |
| `summaryQuery` | Search query that produced the summary |
| `hint` | Advisory message; explains that content was returned because the summary is unavailable (missed or longer than 8000 runes). Only present when falling back to content |

### `flushall`

Clears all cache rows and runs `VACUUM` to reclaim space. No API Key needed.

```bash
deepseek-search-fetch-mcp flushall
```

Output:

```json
{
  "flushed": true
}
```

If run while the server is active, `VACUUM` may wait for the SQLite write lock; on `database is locked`, retry later.

### `apikey`

Manages access keys for MCP clients. These keys are unrelated to the upstream `deepseek_api_key` and only protect `/mcp`; these commands do not require a DeepSeek API Key.

```bash
deepseek-search-fetch-mcp apikey add laptop
deepseek-search-fetch-mcp apikey list
deepseek-search-fetch-mcp apikey remove laptop
```

`apikey add <name>` generates a new key and shows the full value exactly once:

```json
{
  "name": "laptop",
  "api_key": "dsf_...",
  "masked": "dsf_Ab12Cd34…x9Yz",
  "created_at": "2026-11-06T10:00:00Z",
  "auth": "enforced",
  "warning": "访问密钥只显示一次，请立即保存"
}
```

`apikey list` returns only the masked first/last fragments, never plaintext or hashes:

```json
{
  "auth": "enforced",
  "keys": [
    {
      "name": "laptop",
      "masked": "dsf_Ab12Cd34…x9Yz",
      "created_at": "2026-11-06T10:00:00Z",
      "last_used_at": "2026-11-06T10:05:00Z"
    }
  ]
}
```

`apikey remove <name>` hard-deletes a key. Removing the last key sets `auth` to `disabled` with a warning, and `/mcp` immediately stops checking credentials.

- The key store is `auth.db` in the data directory; it contains only SHA-256 hashes and masked fragments.
- `auth.db` is separate from `cache.db`; `flushall` does not clear access keys.
- The server and CLI must use the same data directory, or keys added by the CLI will be invisible to the server.
- Global flags such as `--data-dir` go after `apikey add/remove` and before `<name>`, e.g. `apikey add --data-dir D:\data\deepseek laptop`; alternatively set `DEEPSEEK_SEARCH_FETCH_HOME`.
- Keys are generated only, not imported. If a key is lost, rotate with `remove` + `add`.
- All keys are equivalent in this version; there are no read-only/write scopes or expiry.

### Exit codes and output

- Exit code `0` on success, `1` on failure
- stdout carries JSON only (except `--help`)
- Errors go to stderr
- Logs go only to `~/.deepseek_search_fetch/logs/server.log`

---

## 4. About MCP

MCP (Model Context Protocol) exposes external tools to AI clients that support it. This server runs as **Streamable HTTP**, stateless, with each request independent.

### Endpoints

| Path | Method | Description |
|---|---|---|
| `/mcp` | Streamable HTTP | MCP endpoint |
| `/health` | GET | Health check |
| `/healthz` | GET | Health check |

Health check response:

```json
{"service":"deepseek-search-fetch-mcp","status":"ok"}
```

Default address: `http://127.0.0.1:18081/mcp`

### Access keys, authentication, and rate limiting

- With no access keys, `/mcp` does not authenticate requests and serves them normally; this suits the default `127.0.0.1` local setup.
- As soon as `auth.db` contains at least one key, `/mcp` switches to strict authentication immediately; no restart is needed.
- Clients pass the key with `Authorization: Bearer <key>` or `X-API-Key: <key>`. If both are present and differ, the request is rejected.
- `/health` and `/healthz` are always exempt.
- If the auth store is unavailable, the server returns 503 rather than failing open.

| Scenario | Status | Notes |
|---|---|---|
| Missing, invalid, or conflicting credentials | 401 | Includes `WWW-Authenticate: Bearer` |
| Per-key rate limit or too many auth failures from one IP | 429 | Includes `Retry-After` |
| Auth store unavailable | 503 | Not used by fail2ban |

Rate-limit defaults:

- 10 rps and burst 20 per access key; `rate_limit_rps=0` disables per-key limiting.
- 10 authentication failures per IP in 1 minute, then a 5-minute block; a success resets the counter.
- Per-IP all-request limiting is off by default; enable it with `ip_rate_limit_rps` for public deployments.
- Limits are in-process memory; multiple replicas count independently.

fail2ban can read `event=auth_fail` (401) and `event=auth_blocked` (429) from `server.log`. Do not ban on every 429, or legitimate clients hitting rate limits will be blocked. See [fail2ban deployment](docs/deployment/fail2ban.md) for a filter/jail example.

> Access keys provide authentication only, not transport encryption. Public deployments must sit behind a TLS reverse proxy.

### Client configuration example

Start the server first:

```bash
deepseek-search-fetch-mcp server
```

Then configure the client:

```json
{
  "mcpServers": {
    "deepseek-search-fetch": {
      "type": "http",
      "url": "http://127.0.0.1:18081/mcp",
      "headers": {
        "Authorization": "Bearer dsf_..."
      }
    }
  }
}
```

Omit `headers` when no access keys are configured. Configuration fields differ slightly between clients; follow your client's documentation.

### Tool 1: `web_ext_search`

Extended web search. Supports syntax such as `site:`, `-site:`, `lang:`, `after:`, `before:`, and exact phrases.

When `default_lang` is configured and the query has **no** `lang:` operator, the server appends ` lang:<code>`; queries that already contain `lang:` are left untouched. Completion is skipped if it would exceed 128 characters. Native Windows usually has no `LANG`-style variables, so `auto` resolves to disabled there; set an explicit language code instead.

Input:

| Parameter | Type | Default | Description |
|---|---|---|---|
| `query` | string | required | 1–128 characters, must not be all whitespace |
| `count` | integer | 10 | 1–50; 1–4 is treated as 5; 0 or omitted means 10; negative values or above 50 is an error |
| `ori_question` | string/null | none | Original question that triggered the search, 1–128 characters |
| `localization` | string | empty | IP address used to localize results |

Output:

```json
{
  "results": [
    {
      "title": "Title",
      "url": "https://example.com",
      "snippet": "A 100–300 character excerpt",
      "site_name": "Example",
      "date": "2026-10-23T08:00:00Z",
      "rerank_score": 2.5,
      "authority": 3
    }
  ]
}
```

- Returns only `snippet`, not `summary`, `content`, or `site_icon`
- `snippet` is passed through verbatim without truncation
- Every result's content and summary is written to the cache during search
- **Search never uses cached results**; repeated searches still hit upstream

### Tool 2: `web_ext_fetch`

Cache-first web fetch.

Input:

| Parameter | Type | Default | Description |
|---|---|---|---|
| `url` | string | required | HTTP/HTTPS, must include a host, no userinfo, ≤8192 characters |
| `summary` | boolean | false | When true, prefer the summary cached by search and skip pagination |
| `offset` | integer | 0 | Content page start in runes; ignored when `summary=true` |
| `limit` | integer | 8000 | Content page length, 1–16000; 0 means 8000; ignored when `summary=true` |
| `refresh` | boolean | false | When true, skip the cache read, force a refresh, and discard the old summary |

Content mode output:

```json
{
  "kind": "content",
  "url": "https://example.com/article",
  "title": "Example title",
  "content": "The content fragment returned this time...",
  "contentLength": 14407,
  "offset": 0,
  "length": 8000,
  "hasMore": true,
  "nextOffset": 8000,
  "hasSummary": true,
  "source": "cache",
  "cachedAt": "2026-10-23T08:00:00Z"
}
```

- Slices by rune and never splits UTF-8
- When `offset` exceeds the content length, returns empty `content`, `length=0`, `hasMore=false`
- `nextOffset` appears only when `hasMore=true`

Summary mode hit:

```json
{
  "kind": "summary",
  "url": "https://example.com/article",
  "title": "Example title",
  "summary": "Summary text...",
  "contentLength": 2421,
  "offset": 0,
  "length": 1675,
  "hasSummary": true,
  "summaryQuery": "DeepSeek V4 release",
  "source": "cache",
  "cachedAt": "2026-10-23T08:00:00Z"
}
```

On a summary miss it falls back to content: `kind="content"`, `hasSummary=false`, plus a `hint`. The fallback ignores the requested `offset`/`limit` and returns 8000 characters from position 0.

A cached summary longer than 8000 runes is **never truncated**; it is treated as a miss and content is returned instead, so a returned summary is always complete.

### Recommended workflow

1. Use `web_ext_search` to get candidate URLs and `snippet`s.
2. Call `web_ext_fetch(summary=true)` on candidate URLs. Search already cached the summaries, so this step usually makes no new upstream request.
3. When you need full context, page through content with `web_ext_fetch` using `offset`/`limit`.
4. Pass `refresh=true` when you need fresh content.

---

## 5. Cache Mechanics

### Storage

- File: `~/.deepseek_search_fetch/cache.db`
- The access-key store is a separate `auth.db` and is not part of the cache; `flushall` does not clear it
- Engine: SQLite, WAL mode, `busy_timeout=5000`, `synchronous=NORMAL`
- Each process uses a single serialized connection
- When multiple processes/sessions share the file, SQLite handles concurrency

Main fields of the `web_cache` table:

| Column | Description |
|---|---|
| `url` | Normalized URL, primary key |
| `title` | Page title |
| `content` | Full content |
| `summary` | Condensed content returned by search |
| `summary_query` | Search query that produced the summary; may include an auto-appended `lang:xx` when `default_lang` is configured |
| `content_source` | `search` or `fetch` |
| `content_at` / `summary_at` | Write timestamps for content/summary respectively |
| `last_used` | Last use time (read or write), used for LRU eviction |
| `size_bytes` | Historical field: sum of title/content/summary bytes; kept for inspection only and not used for capacity decisions |

### URL normalization

Cache keys drop the fragment and lowercase the scheme and host; path and query are kept as-is.

For example:

- `https://EXAMPLE.com/a#section`
- `https://example.com/a#other`

both map to `https://example.com/a`.

The `url` field in `web_ext_fetch` responses also uses the normalized value, so the same URL produces the same output whether `source=upstream` or `source=cache`.

### TTL and capacity

- `cache_ttl`: defaults to 30 minutes; content and summary are judged stale by their own timestamps
- `cache_max_bytes`: defaults to 1 GiB; the limit is checked against the **page-level live size**, and least-recently-used entries are batch-evicted by `last_used` when exceeded
- Expired cleanup runs once at startup; during runtime it fires at most once per minute after a write
- TTL cleanup: a row is deleted only when **both** content and summary are expired
- Capacity eviction: when the live size `(page_count - freelist_count) × page_size` exceeds `cache_max_bytes`, rows are deleted oldest-first in batches until the cache drops to 95% of the limit; deletion granularity is pages, so removing a few small rows may not lower the metric immediately
- `cache_max_bytes` is a **logical live-page limit, not a disk-usage limit**: `cache.db` and `cache.db-wal` can be noticeably larger, and only the VACUUM in `flushall` reclaims file space
- A read hit updates `last_used`

### How search writes the cache

The flow for `web_ext_search` / CLI `search`:

```text
Call upstream /v1/tools/search (with need_content=true when the cache is available)
  └─ For each result write:
       url, title, content, summary, summary_query=this query
```

Write rules:

- An existing **unexpired fetch content** is never overwritten by search content, regardless of length
- Otherwise the content returned by search is written with `content_source=search`
- Summary: when search returns a non-empty summary it overwrites the old one and records `summary_query`; when it returns none, an unexpired old summary is kept
- Results with neither content nor summary are skipped to avoid empty rows
- All results are written in a single transaction

**Search never replaces the upstream request with cached results.** Every search queries upstream; writes read existing entries to merge (so unexpired fetch content is preserved), but a cache hit never skips the search.

### How fetch uses the cache

Content mode:

```text
web_ext_fetch(summary=false, refresh=false)
  ├─ Unexpired content hit → slice by offset/limit, source=cache
  └─ Miss → query upstream → write cache → slice, source=upstream

web_ext_fetch(summary=false, refresh=true)
  └─ Skip cache → query upstream → write cache (and discard the old summary) → slice
```

Summary mode:

```text
web_ext_fetch(summary=true, refresh=false)
  ├─ Unexpired summary hit → return the summary, source=cache
  ├─ Summary miss but content hit → return content, hasSummary=false
  └─ Both miss → query upstream → write content cache → return content, hasSummary=false

web_ext_fetch(summary=true, refresh=true)
  └─ Skip cache → query upstream → write content cache and discard the old summary
     → return content, hasSummary=false
```

Write rules:

- When fetch returns non-empty content it always overwrites, with `content_source=fetch`
- An unexpired old summary is kept by default
- `refresh=true` clears the old summary; this happens even when the upstream returns an empty body and no new content is written
- Nothing is written when upstream returns empty content

### When the cache is unavailable

- `server` fails to open the cache at startup: logs a warning and keeps running without a cache
- CLI `search` / `fetch` fails to open the cache: logs a warning to the log file and continues
- MCP `web_ext_search` does not request `need_content=true` when the cache is unavailable
- `flushall` returns an error directly when the cache cannot be opened

### Notes

- Summaries are keyed by URL; when a URL is searched with different queries, the later non-empty summary overwrites the earlier one
- `summaryQuery` shows which search produced the summary; a summary does not necessarily match the current question exactly
- Cache contents are persisted, kept for up to `cache_ttl` by default (evicted sooner when capacity is tight); use `flushall` to clear immediately
- If an entry is evicted mid-pagination, later pages refetch and `offset` may shift

---

## 6. Troubleshooting

| Symptom | Action |
|---|---|
| `必须指定命令：server、search、fetch、flushall 或 apikey` | Add a subcommand, e.g. `server` |
| `未配置 DeepSeek API Key` | Set `DEEPSEEK_API_KEY`, `--deepseek-api-key`, or write it to the config file |
| `打开访问密钥库失败` | Check the data directory and `auth.db` write permissions; the server refuses to start rather than degrading |
| MCP returns 401 | Access keys exist but the client omitted or sent the wrong credential; inspect names with `apikey list` and configure `Authorization: Bearer <key>` |
| MCP returns 429 | Per-key rate limit or per-IP auth-failure block; wait for `Retry-After` or tune `rate_limit_*` / `auth_fail_*` |
| `缓存不可用` in the logs | Check the user home directory and write permissions; the service falls back to no-cache mode |
| `fetch` times out | Upstream browser rendering may be slow; `fetch_timeout` defaults to 60s, and upstream may itself return 504 |
| Search results are too long | Search returns only `snippet`; use `fetch(summary=true)` for a summary, or `fetch` for content (CLI returns the full text, MCP paginates via `offset`/`limit`) |
| Cache is using too much space | Lower `cache_max_bytes`, or run `flushall` |
| Repeated searches still cost money | Search never uses cached results and always queries upstream; the cache only saves fetches |

Security recommendations:

- With no access keys, `/mcp` has no authentication; before exposing the service, run `apikey add <name>` and confirm `apikey list` reports `auth=enforced`
- Access keys are stored as hashes only; the full value is shown once by `apikey add`. `auth.db` is separate from `cache.db`, and `flushall` does not affect keys
- Public deployments must sit behind a TLS reverse proxy; access keys sent over plain HTTP are exposed
- Configure fail2ban on the reverse proxy or host to ban on `auth_fail` / `auth_blocked`; see [fail2ban deployment](docs/deployment/fail2ban.md)
- Keep the DeepSeek API Key in environment variables or the config file only; never commit it
- Cached page content may contain sensitive material; run `flushall` when necessary

---

## 7. Version and Build Channel

- Local `go build`: `version=dev`, `build=local`
- Local with explicit version: `-ldflags "-X main.version=v1.2.3"`, channel stays `local`
- Docker/GHCR: injects `build=ghcr`, version taken from the build arg `VERSION`
- The MCP handshake version is `version+build`, e.g. `dev+local`, `v1.2.3+ghcr`

## 8. Development

```bash
go build ./...
go vet ./...
gofmt -l .
go test ./...
CGO_ENABLED=1 go test -race ./...
```

All tests use `httptest` fake upstreams and never touch the real network.

## 9. License

[MIT](LICENSE)
