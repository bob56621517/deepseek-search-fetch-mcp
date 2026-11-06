# deepseek-search-fetch-mcp

中文 | [English](README.en.md)

把 DeepSeek 官方的网页搜索与抓取能力暴露为 MCP 服务，同时提供一个可直接调用的 CLI。项目自身不实现检索逻辑，只负责 MCP 协议、工具描述、参数校验、凭据注入与本地缓存。

- **MCP 工具**：`web_ext_search`、`web_ext_fetch`
- **CLI 子命令**：`server`、`search`、`fetch`、`flushall`
- **缓存**：SQLite，搜索预热、抓取复用，支持 TTL、容量淘汰与一键清空
- **传输**：Streamable HTTP，无状态，JSON 响应
- **默认监听**：`127.0.0.1:18081`

> 使用前请先在 DeepSeek 平台获取 API Key。

---

## 1. 安装与运行

### 环境要求

- Go 1.25 或更高版本
- 不需要 CGO：SQLite 使用纯 Go 驱动 `modernc.org/sqlite`
- 国内网络建议先设置 Go 代理：

```bash
export GOPROXY=https://goproxy.cn,direct
```

### 从 Go 源码构建（本地使用）

```bash
git clone https://github.com/bob56621517/deepseek-search-fetch-mcp.git
cd deepseek-search-fetch-mcp

# Linux / macOS
go build -o deepseek-search-fetch-mcp ./cmd/deepseek-search-fetch-mcp

# Windows
go build -o deepseek-search-fetch-mcp.exe ./cmd/deepseek-search-fetch-mcp
```

二进制会生成在当前目录，直接运行即可：

```bash
./deepseek-search-fetch-mcp server        # Linux / macOS
.\deepseek-search-fetch-mcp.exe server    # Windows
```

如需指定版本号，可加 `-ldflags "-X main.version=v1.2.3"`（不传则为 `dev+local`）。

### 使用 go install（一条命令下载并安装）

`go install` 会从模块代理下载源码、编译，并把可执行文件安装到 `$(go env GOPATH)/bin`：

```bash
go install github.com/bob56621517/deepseek-search-fetch-mcp/cmd/deepseek-search-fetch-mcp@latest
```

- `@latest` 取最新版本；也可写 `@v1.2.3` 指定版本，或 `@main` 取主分支
- 产物位于 `$(go env GOPATH)/bin`（Windows 通常是 `%USERPROFILE%\go\bin`）
- 把该目录加入 `PATH` 后即可直接运行：

```bash
export PATH="$(go env GOPATH)/bin:$PATH"   # Linux / macOS
deepseek-search-fetch-mcp server
```

```powershell
$env:Path += ";$(go env GOPATH)\bin"       # Windows PowerShell
deepseek-search-fetch-mcp.exe server
```

### 启动 MCP 服务

```bash
deepseek-search-fetch-mcp server --port 18081
```

**必须显式指定 `server` 子命令；不带任何参数会报错并打印用法。**

### Docker

```bash
docker build -t deepseek-search-fetch-mcp .

docker run -d --name deepseek-search-fetch-mcp \
  -p 18081:18081 \
  -e DEEPSEEK_API_KEY=sk-... \
  -v deepseek-sf-data:/data \
  deepseek-search-fetch-mcp
```

- 容器内 `HOME=/data`，配置、日志、缓存都会落在 `/data/.deepseek_search_fetch`。
- 不挂载具名卷时，Docker 会使用匿名卷；容器重建后数据不便复用，建议始终挂载 `/data`。
- 容器默认执行 `server`；如需在容器内执行 CLI，覆盖命令即可：

```bash
docker run --rm -e DEEPSEEK_API_KEY=sk-... \
  -v deepseek-sf-data:/data \
  deepseek-search-fetch-mcp search "DeepSeek V4"
```

镜像同时发布到 GHCR：`ghcr.io/bob56621517/deepseek-search-fetch-mcp`。

### Windows 服务

以 Windows 服务方式开机自启，见 [Windows 服务部署](docs/deployment/windows-service.md)。

### 安装路径

| 内容 | 路径 |
|---|---|
| 配置 | `~/.deepseek_search_fetch/config.json` |
| 日志 | `~/.deepseek_search_fetch/logs/server.log` |
| 缓存 | `~/.deepseek_search_fetch/cache.db`（同目录可能出现 `-wal`、`-shm`） |

Windows 下 `~` 对应用户目录，例如 `C:\Users\<name>`。

数据目录默认是 `~/.deepseek_search_fetch`，可用 `--data-dir` 或环境变量 `DEEPSEEK_SEARCH_FETCH_HOME` 覆盖（优先级：启动参数 > 环境变量 > 默认值）。配置、缓存、日志始终落在同一个数据目录下。

- 首次启动时自动创建目录与配置文件模板；Key 为空。
- 配置只在进程启动时读取一次，修改后需要重启服务。CLI 每次执行都会重新读取。
- 如果无法确定用户主目录且未指定数据目录，程序会跳过配置文件创建，仅使用环境变量与启动参数；此时缓存不可用。以服务、容器等非交互账户运行时，务必显式指定数据目录或保证家目录指向用户目录（见 [Windows 服务部署](docs/deployment/windows-service.md)）。

### 日志

- 服务日志文件：`~/.deepseek_search_fetch/logs/server.log`
- 滚动策略：单文件 10 MiB，最多保留 5 个文件（当前 `server.log` + 4 个备份），备份名为 `server-<时间戳>.log`
- `server` 模式：日志同时输出到 stderr 与文件
- CLI 模式：日志**只写入文件**，stdout 只输出 JSON
- 如果无法确定用户主目录：`server` 模式只输出到 stderr，CLI 模式丢弃日志

---

## 2. 配置

配置文件默认位于 `~/.deepseek_search_fetch/config.json`，数据目录可用 `--data-dir` 或 `DEEPSEEK_SEARCH_FETCH_HOME` 覆盖：

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
  "http_max_idle_conns": 5,
  "http_max_idle_conns_per_host": 5,
  "http_idle_conn_timeout": "90s",
  "http_tls_handshake_timeout": "10s",
  "reset_connection_on_error": true
}
```

### 配置优先级

**命令行参数 > 环境变量 > 配置文件 > 默认值**

### 配置项

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `deepseek_api_key` | 空 | DeepSeek API Key；`server`、`search`、`fetch` 必需，`flushall` 不需要 |
| `deepseek_api_url` | `https://api.deepseek.com` | 上游 Base URL |
| `server_host` | `127.0.0.1` | `server` 模式监听地址 |
| `server_port` | `18081` | `server` 模式监听端口 |
| `request_timeout` | `30s` | 搜索等常规上游请求超时 |
| `fetch_timeout` | `60s` | 抓取请求超时；浏览器渲染较慢，独立配置 |
| `cache_ttl` | `30m` | 缓存条目有效期，正文与摘要分别计算 |
| `cache_max_bytes` | `1073741824`（1 GiB） | 页级活跃大小上限，最小 1 MiB，不代表磁盘占用 |
| `default_lang` | 空 | 默认语言补全：空为关闭，`auto` 读取 `LC_ALL`/`LC_MESSAGES`/`LANG`，也可写两位语言码（如 `zh`）；写 `zh-CN` 会报错 |
| `http_max_idle_conns` | `5` | 全局最大空闲连接数 |
| `http_max_idle_conns_per_host` | `5` | 单主机最大空闲连接数 |
| `http_idle_conn_timeout` | `90s` | 空闲连接超时 |
| `http_tls_handshake_timeout` | `10s` | TLS 握手超时 |
| `reset_connection_on_error` | `true` | 任意上游调用失败后丢弃空闲连接，下次重新建连 |

### 环境变量

| 环境变量 | 对应配置 |
|---|---|
| `DEEPSEEK_API_KEY` | `deepseek_api_key` |
| `DEEPSEEK_API_URL` | `deepseek_api_url` |
| `DEEPSEEK_SEARCH_FETCH_HOME` | 数据目录（默认 `~/.deepseek_search_fetch`） |
| `DEEPSEEK_SEARCH_FETCH_HOST` | `server_host` |
| `DEEPSEEK_SEARCH_FETCH_PORT` | `server_port` |

### 命令行参数

全局参数在所有子命令中都可用，但必须写在**子命令之后**：

```bash
deepseek-search-fetch-mcp server --deepseek-api-key sk-... --port 18081
deepseek-search-fetch-mcp search --deepseek-api-key sk-... "query"
```

| 参数 | 适用命令 | 说明 |
|---|---|---|
| `--deepseek-api-key` | 全部 | 覆盖 API Key |
| `--deepseek-api-url` | 全部 | 覆盖 Base URL |
| `--data-dir` | 全部 | 数据目录，覆盖默认 `~/.deepseek_search_fetch` |
| `--host` | `server` | 监听地址 |
| `--port` | `server` | 监听端口 |
| `--count` | `search` | 返回条数，默认 10 |
| `--content` | `search` | 额外返回网页正文 |
| `--summary` | `fetch` | 优先返回缓存摘要 |
| `--refresh` | `fetch` | 跳过缓存并强制刷新 |

> Go 的 `flag` 解析在遇到第一个位置参数后停止，因此选项要写在 `<query>` / `<url>` 之前。

---

## 3. CLI 用法

```bash
deepseek-search-fetch-mcp server   [--host 127.0.0.1] [--port 18081]
deepseek-search-fetch-mcp search   [--count 10] [--content] <query>
deepseek-search-fetch-mcp fetch    [--summary] [--refresh] <url>
deepseek-search-fetch-mcp flushall
deepseek-search-fetch-mcp --help
```

### `server`

启动 MCP 服务。默认 `127.0.0.1:18081`。

```bash
deepseek-search-fetch-mcp server
deepseek-search-fetch-mcp server --host 0.0.0.0 --port 18081
```

### `search`

直接调用上游搜索，输出 JSON。

- 默认返回 `summary`，不返回正文
- `--content` 额外返回 `content`
- `--count` 默认 10；0 或省略为 10；1–4 按 5 处理；负数或大于 50 报错
- 结果会写入缓存；**搜索本身不使用缓存结果**（每次都请求上游）
- 参数写在 query 之前

```bash
deepseek-search-fetch-mcp search --count 10 "DeepSeek V4 发布"
deepseek-search-fetch-mcp search --count 5 --content "Go 1.25 release notes"
```

输出示例（`--content` 时每条结果额外带 `content`）：

```json
{
  "results": [
    {
      "title": "示例标题",
      "url": "https://example.com/article",
      "snippet": "与查询相关的短摘要",
      "summary": "页面正文的精简版，通常 1–6k 字符",
      "site_name": "Example",
      "date": "2026-10-23T08:00:00Z",
      "rerank_score": 2.5,
      "authority": 3
    }
  ]
}
```

### `fetch`

直接调用抓取流程，输出 JSON。

- 缓存优先；未命中才请求上游
- **正文模式不分页**，始终返回完整正文
- `--summary`：优先返回搜索缓存的摘要；没有摘要时回退为完整正文
- `--refresh`：跳过缓存读取，重新抓取上游，并丢弃该 URL 的旧摘要

```bash
# 完整正文
deepseek-search-fetch-mcp fetch "https://example.com/article"

# 优先读取缓存摘要
deepseek-search-fetch-mcp fetch --summary "https://example.com/article"

# 强制刷新
deepseek-search-fetch-mcp fetch --refresh "https://example.com/article"
```

完整正文输出示例：

```json
{
  "kind": "content",
  "url": "https://example.com/article",
  "title": "示例标题",
  "content": "完整 Markdown 正文……",
  "contentLength": 14407,
  "offset": 0,
  "length": 14407,
  "hasMore": false,
  "hasSummary": true,
  "source": "cache",
  "cachedAt": "2026-10-23T08:00:00Z"
}
```

摘要命中示例：

```json
{
  "kind": "summary",
  "url": "https://example.com/article",
  "title": "示例标题",
  "summary": "页面正文的精简版……",
  "contentLength": 2421,
  "offset": 0,
  "length": 1675,
  "hasSummary": true,
  "summaryQuery": "DeepSeek V4 发布",
  "source": "cache",
  "cachedAt": "2026-10-23T08:00:00Z"
}
```

摘要缺失时会回退为正文：

```json
{
  "kind": "content",
  "hasSummary": false,
  "hint": "该 URL 没有可用的缓存摘要；已返回正文。如需摘要，请先调用 web_ext_search。"
}
```

字段说明：

| 字段 | 说明 |
|---|---|
| `kind` | `content` 或 `summary` |
| `contentLength` | 正文总字符数（rune） |
| `offset` | 起始偏移；摘要与 CLI 完整正文固定为 0 |
| `length` | 本次返回的字符数 |
| `hasMore` | 正文模式：是否还有下一页；CLI 完整正文恒为 `false`；摘要模式无此字段 |
| `nextOffset` | 下一页偏移，读完时省略；仅 MCP 分页模式出现 |
| `hasSummary` | 是否存在可用的缓存摘要 |
| `source` | `cache`（命中缓存）或 `upstream`（实时抓取） |
| `cachedAt` | 缓存时间，UTC RFC 3339 |
| `summaryQuery` | 生成该摘要的搜索词 |
| `hint` | 提示信息；摘要不可用（未命中或超过 8000 个字符）时说明已回退正文。仅出现在回退为正文的输出中 |

### `flushall`

清空所有缓存行并执行 `VACUUM` 回收空间，不需要 API Key。

```bash
deepseek-search-fetch-mcp flushall
```

输出：

```json
{
  "flushed": true
}
```

服务运行期间执行时，`VACUUM` 可能等待 SQLite 写锁；如遇 `database is locked`，稍后重试即可。

### 退出码与输出

- 成功退出码 `0`，失败退出码 `1`
- stdout 只输出 JSON（`--help` 除外）
- 错误信息写入 stderr
- 日志只写入 `~/.deepseek_search_fetch/logs/server.log`

---

## 4. MCP 介绍

MCP（Model Context Protocol）用于把外部工具暴露给支持该协议的 AI 客户端。本服务以 **Streamable HTTP** 形式运行，无状态，每个请求独立。

### 端点

| 路径 | 方法 | 说明 |
|---|---|---|
| `/mcp` | Streamable HTTP | MCP 端点 |
| `/health` | GET | 健康检查 |
| `/healthz` | GET | 健康检查 |

健康检查返回：

```json
{"service":"deepseek-search-fetch-mcp","status":"ok"}
```

默认地址：`http://127.0.0.1:18081/mcp`

### 客户端配置示例

先启动服务：

```bash
deepseek-search-fetch-mcp server
```

再在客户端配置：

```json
{
  "mcpServers": {
    "deepseek-search-fetch": {
      "type": "http",
      "url": "http://127.0.0.1:18081/mcp"
    }
  }
}
```

不同客户端的配置字段可能略有差异，以客户端文档为准。

### 工具一：`web_ext_search`

扩展网页搜索。支持 `site:`、`-site:`、`lang:`、`after:`、`before:`、精确短语等语法。

配置 `default_lang` 后，query 中**没有** `lang:` 操作符时，服务端会按配置追加 ` lang:<code>`；已经写了 `lang:` 的 query 不会被改动。补全后超过 128 字符时不补全。原生 Windows 默认没有 `LANG` 等环境变量，`auto` 会关闭，建议显式配置语言码。

输入：

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `query` | string | 必填 | 1–128 字符，不能全为空白 |
| `count` | integer | 10 | 1–50；1–4 按 5 处理；0 或省略为 10；负数或大于 50 报错 |
| `ori_question` | string/null | 无 | 触发搜索的原始问题，1–128 字符 |
| `localization` | string | 空 | 用于本地化结果的 IP 地址 |

输出：

```json
{
  "results": [
    {
      "title": "标题",
      "url": "https://example.com",
      "snippet": "100–300 字符的短摘要",
      "site_name": "Example",
      "date": "2026-10-23T08:00:00Z",
      "rerank_score": 2.5,
      "authority": 3
    }
  ]
}
```

- 只返回 `snippet`，不返回 `summary`、`content`、`site_icon`
- `snippet` 原样输出，不做截断
- 搜索时会把每条结果的正文与摘要写入缓存
- **搜索不使用缓存结果**；重复搜索仍会请求上游

### 工具二：`web_ext_fetch`

缓存优先的网页抓取。

输入：

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `url` | string | 必填 | HTTP/HTTPS，含主机名，不能带用户名/密码，≤8192 字符 |
| `summary` | boolean | false | true 时优先返回搜索缓存的摘要，不分页 |
| `offset` | integer | 0 | 正文分页起点，按 rune 计；`summary=true` 时忽略 |
| `limit` | integer | 8000 | 正文分页长度，1–16000；0 视为 8000；`summary=true` 时忽略 |
| `refresh` | boolean | false | true 时跳过缓存读取，强制刷新并丢弃旧摘要 |

正文模式输出：

```json
{
  "kind": "content",
  "url": "https://example.com/article",
  "title": "示例标题",
  "content": "本次返回的正文片段……",
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

- 按 rune 切分，不会切断 UTF-8
- `offset` 超过正文长度时返回空 `content`、`length=0`、`hasMore=false`
- `nextOffset` 仅在 `hasMore=true` 时出现

摘要模式命中：

```json
{
  "kind": "summary",
  "url": "https://example.com/article",
  "title": "示例标题",
  "summary": "摘要内容……",
  "contentLength": 2421,
  "offset": 0,
  "length": 1675,
  "hasSummary": true,
  "summaryQuery": "DeepSeek V4 发布",
  "source": "cache",
  "cachedAt": "2026-10-23T08:00:00Z"
}
```

摘要未命中时回退为正文：`kind="content"`、`hasSummary=false`，并带 `hint`。回退时忽略请求中的 `offset`/`limit`，从第 0 字符返回默认 8000 字符。

缓存摘要超过 8000 个字符时**不会截断**，而是按未命中摘要处理并回退为正文，保证返回的摘要始终是完整内容。

### 推荐使用流程

1. 用 `web_ext_search` 拿到候选 URL 与 `snippet`。
2. 对候选 URL 调用 `web_ext_fetch(summary=true)`。搜索已经把摘要写入缓存，这一步通常不会产生新的上游请求。
3. 需要完整上下文时，用 `web_ext_fetch` 配合 `offset`/`limit` 分页读取正文。
4. 需要新鲜内容时传 `refresh=true`。

---

## 5. 缓存机制

### 存储

- 文件：`~/.deepseek_search_fetch/cache.db`
- 引擎：SQLite，WAL 模式，`busy_timeout=5000`，`synchronous=NORMAL`
- 每个进程使用单连接串行访问
- 多个进程/会话使用同一文件时由 SQLite 处理并发

缓存表 `web_cache` 的主要字段：

| 字段 | 说明 |
|---|---|
| `url` | 规范化 URL，主键 |
| `title` | 页面标题 |
| `content` | 完整正文 |
| `summary` | 搜索返回的正文精简版 |
| `summary_query` | 生成摘要的搜索词；配置 `default_lang` 时可能包含自动补全的 `lang:xx` |
| `content_source` | `search` 或 `fetch` |
| `content_at` / `summary_at` | 正文/摘要各自的写入时间 |
| `last_used` | 最近使用时间（读或写），用于 LRU 淘汰 |
| `size_bytes` | 历史字段，按 title/content/summary 的字节数累加；仅用于排查，不参与容量判断 |

### URL 规范化

缓存键会去掉 fragment，并把 scheme 与 host 统一为小写；path 与 query 保持原样。

例如：

- `https://EXAMPLE.com/a#section`
- `https://example.com/a#other`

都会映射到 `https://example.com/a`。

`web_ext_fetch` 响应中的 `url` 也使用规范化后的值，因此同一 URL 无论 `source=upstream` 还是 `source=cache`，输出保持一致。

### TTL 与容量

- `cache_ttl`：默认 30 分钟，正文与摘要分别按各自时间戳判断是否过期
- `cache_max_bytes`：默认 1 GiB，按**页级活跃大小**判断是否超限，超过后按 `last_used` 批量淘汰最久未使用的条目
- 过期清理在启动时执行一次；运行期间写入后最多每分钟触发一次
- TTL 清理：只有正文与摘要**都过期**时才删除整行
- 容量淘汰：活跃大小 `(page_count - freelist_count) × page_size` 超过 `cache_max_bytes` 时，从最久未使用的条目开始批量删除，直到降到上限的 95%；删除粒度是页，删掉少量小行可能不会立即降低指标
- `cache_max_bytes` 是**逻辑活跃页上限，不是磁盘占用上限**：`cache.db` 与 `cache.db-wal` 可能明显更大，只有 `flushall` 的 VACUUM 会回收文件空间
- 读取命中会更新 `last_used`

### search 如何写缓存

`web_ext_search` / CLI `search` 的流程：

```text
调用上游 /v1/tools/search（缓存可用时携带 need_content=true）
  └─ 对每条结果写入：
       url、title、content、summary、summary_query=本次 query
```

写入规则：

- 若已有**未过期的 fetch 正文**，不会被 search 正文覆盖（无论长短）
- 否则，用 search 返回的正文写入，`content_source=search`
- 摘要：搜索返回非空摘要时，覆盖旧摘要并记录 `summary_query`；搜索未返回摘要时保留未过期的旧摘要
- 既无正文也无摘要的结果跳过写入，避免留下空行
- 所有结果在同一个事务中写入

**搜索不会用缓存结果替代上游请求。** 每次搜索都会请求上游；写入时会读取已有条目做合并（避免覆盖未过期的 fetch 正文），但不会因为缓存命中而跳过搜索。

### fetch 如何使用缓存

正文模式：

```text
web_ext_fetch(summary=false, refresh=false)
  ├─ 命中未过期正文 → 按 offset/limit 切片返回，source=cache
  └─ 未命中 → 请求上游 → 写入缓存 → 切片返回，source=upstream

web_ext_fetch(summary=false, refresh=true)
  └─ 跳过缓存 → 请求上游 → 写入缓存（同时丢弃旧摘要）→ 切片返回
```

摘要模式：

```text
web_ext_fetch(summary=true, refresh=false)
  ├─ 命中未过期摘要 → 直接返回摘要，source=cache
  ├─ 未命中摘要，但命中正文 → 返回正文，hasSummary=false
  └─ 都未命中 → 请求上游 → 写入正文缓存 → 返回正文，hasSummary=false

web_ext_fetch(summary=true, refresh=true)
  └─ 跳过缓存 → 请求上游 → 写入正文缓存并丢弃旧摘要
     → 返回正文，hasSummary=false
```

写入规则：

- fetch 返回非空正文时，总是覆盖旧正文，`content_source=fetch`
- 默认保留未过期的旧摘要
- `refresh=true` 时清空旧摘要；即使上游返回空正文、未写入新正文，旧摘要也会被清除
- 上游返回空正文时不写缓存

### 缓存不可用时

- `server` 启动时缓存打开失败：记录 warning，服务继续以无缓存模式运行
- CLI `search` / `fetch` 缓存打开失败：记录 warning 到日志文件，命令继续执行
- MCP `web_ext_search` 在缓存不可用时不会请求 `need_content=true`
- `flushall` 缓存打开失败时直接返回错误

### 注意事项

- 摘要以 URL 为键，同一 URL 被不同 query 搜索时，后一次的非空摘要会覆盖前一次
- `summaryQuery` 标明摘要来自哪次搜索；摘要不保证与当前问题精确对应
- 缓存内容会落盘，默认最多保留 `cache_ttl`（容量不足时会更早淘汰）；可用 `flushall` 立即清空
- 分页过程中条目被淘汰时，后续页会重新抓取，`offset` 可能发生偏移

---

## 6. 排查

| 现象 | 处理 |
|---|---|
| `必须指定命令：server、search、fetch 或 flushall` | 加上子命令，例如 `server` |
| `未配置 DeepSeek API Key` | 设置 `DEEPSEEK_API_KEY`、`--deepseek-api-key` 或写入配置文件 |
| 日志出现 `缓存不可用` | 检查用户主目录与写权限；服务会降级为无缓存模式 |
| `fetch` 超时 | 上游浏览器渲染可能较慢；`fetch_timeout` 默认 60s，上游自身也可能返回 504 |
| 搜索结果很长 | 搜索只返回 `snippet`；需要摘要用 `fetch(summary=true)`，需要正文用 `fetch`（CLI 返回全文，MCP 按 `offset`/`limit` 分页） |
| 缓存占用过大 | 调小 `cache_max_bytes`，或执行 `flushall` |
| 重复搜索仍产生费用 | 搜索不使用缓存结果，每次都会请求上游；缓存只节省 fetch |

安全建议：

- MCP 端点没有鉴权，默认只监听 `127.0.0.1`，不要直接暴露到公网
- API Key 只放在环境变量或配置文件中，不要提交到仓库
- 缓存的网页正文可能包含敏感内容，必要时执行 `flushall`

---

## 7. 版本与构建渠道

- 本地 `go build`：`version=dev`、`build=local`
- 本地指定版本：`-ldflags "-X main.version=v1.2.3"`，渠道仍为 `local`
- Docker/GHCR：注入 `build=ghcr`，版本取构建参数 `VERSION`
- MCP 握手版本为 `version+build`，例如 `dev+local`、`v1.2.3+ghcr`

## 8. 开发

```bash
go build ./...
go vet ./...
gofmt -l .
go test ./...
CGO_ENABLED=1 go test -race ./...
```

测试全部使用 `httptest` 假上游，不访问真实网络。

## 9. 许可证

[MIT](LICENSE)
