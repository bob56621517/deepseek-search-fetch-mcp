# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 语言约定

- 文档、代码注释、提交信息、PR/MR 描述使用中文。
- 由英文模板生成的文件(README 模板、Issue/PR 模板、注释模板等)落地前翻译为中文。
- 仅当符号含义强于语言含义时保留原文:标识符、关键字、协议与格式名、配置键、命令、URL、module path(如 `MCP`、Streamable HTTP、`ghcr.io`、`github.com/bob56621517/deepseek-search-fetch-mcp`)。

## 项目约定

- Go 项目,module 为 `github.com/bob56621517/deepseek-search-fetch-mcp`。
- 同一可执行文件有两种形态:MCP server(`server` 子命令)与 CLI(`search` / `fetch` / `flushall` / `apikey`)。
- MCP server 使用官方 Go SDK `github.com/modelcontextprotocol/go-sdk`,以 Streamable HTTP 形式暴露。
- 只暴露 DeepSeek 官方的两个工具:`web_ext_search`、`web_ext_fetch`。访问密钥管理只通过 CLI `apikey` 提供,不暴露为 MCP 工具。
- 凭据分两类:上游 DeepSeek API Key(`deepseek_api_key`)与 MCP 客户端访问密钥(`auth.db`)。访问密钥只保护 `/mcp`,与上游 Key 无关。
- 访问密钥只保存 SHA-256 哈希与首尾片段,完整值只在 `apikey add` 时显示一次;`apikey list` 显示首尾脱敏。
- 没有访问密钥时 `/mcp` 不鉴权;至少有一个密钥时强制校验;删除最后一个密钥后立即回到不鉴权模式。
- 访问密钥库 `auth.db` 与缓存 `cache.db` 分离;`flushall` 不影响访问密钥。
- 鉴权失败返回 401,限流/失败超限返回 429,鉴权库不可用返回 503;日志使用 `event`/`remote`/`status`/`reason` 等稳定字段,供 fail2ban 匹配。
- `web_ext_search` 只返回 `snippet`,没有 `summary` 参数;搜索时固定向上游请求 `need_content=true`,把每条结果的 `content`、`summary` 写入 SQLite 缓存。
- 搜索 `count` 默认 10;低于 5 自动按 5 处理;超过 50 报错。
- `web_ext_fetch` 缓存优先,支持 `summary`(读取搜索缓存的摘要,不分页)、`offset`/`limit`(正文按 rune 分页)与 `refresh`(跳过缓存强制刷新)。
- 缓存使用 SQLite,文件为 `~/.deepseek_search_fetch/cache.db`;TTL 默认 30 分钟、容量默认 1 GiB,均可配置;容量按页级活跃大小判断,不是磁盘占用。缓存故障只记日志,不影响主功能。
- 接口契约以 `api/openapi.yaml` 为准;缓存与 CLI 属于包装层行为,不改变上游契约。
- 默认监听端口 `18081`。
- 源码发布到 GitHub;容器镜像发布到 GHCR。
- `origin`(GitHub)是主远端;`gitlab` 为备用远端,不是默认推送目标。需要两端一致时使用仓库内已配置的 `git sync` 别名。

## 分支与协作流程

`main` 是保护分支(ruleset `protect-main`):禁止直接 push、禁止 force push、禁止删除,所有改动都必须通过 PR 进入。要求的批准数为 0(单人项目自审自合),但 agent **不得自行合并 PR**,必须等维护者明确同意。

标准流程:

1. 开分支:从最新 `main` 拉出工作分支(无议题时直接用 `dev`,见下)。
2. push:把工作分支推到 `origin`。
3. 创建 PR:`gh pr create`,目标分支为 `main`。
4. 等待审核:PR 创建后停下,交给维护者审核。
5. 合并:维护者同意后用 `gh pr merge --squash --delete-branch` 合并并删除远程分支。

分支命名:

- **有关联议题时**:分支名必须以议题编号开头,格式 `<议题编号>-<类型>-<简短描述>`,类型取 `feat`、`fix`、`refactor`,必要时可用 `docs`、`chore`、`test`。例如议题 #6 做修复对应 `6-fix-cache-ttl`,做新功能对应 `6-feat-data-dir`,做重构对应 `6-refactor-cache`。
- **没有议题时**:默认直接在 `dev` 分支上开发,不必另开分支。
- 历史上的 `feat/`、`fix/`、`refactor/` 前缀分支不受此约定约束,新工作不再使用 `类型/描述` 这套旧命名。

`dev` 分支:

- `dev` 是没有议题时的长期工作分支,可直接 push,但仍需走 `dev` → `main` 的 PR。
- 每次在 `dev` 上开始新工作前,先把最新 `main` 合并进来:

```bash
git switch dev
git fetch origin
git merge origin/main
```

- 冲突在 `dev` 上解决,解决后照常 push 并发起 PR。

## 配置与日志

默认配置文件:`~/.deepseek_search_fetch/config.json`(数据目录可用 `--data-dir` 或 `DEEPSEEK_SEARCH_FETCH_HOME` 覆盖)

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

首次启动时如果目录或文件不存在,会自动创建(Key 为空)。配置只在启动时读取一次,修改后需要重启。配置优先级:**启动参数 > 环境变量 > 配置文件 > 默认值**。访问密钥通过 `apikey` 命令管理,增删立即生效,不需要重启。

- 启动参数:`--deepseek-api-key`、`--deepseek-api-url`、`--data-dir`、`--host`、`--port`
- 环境变量:`DEEPSEEK_API_KEY`、`DEEPSEEK_API_URL`、`DEEPSEEK_SEARCH_FETCH_HOME`、`DEEPSEEK_SEARCH_FETCH_HOST`、`DEEPSEEK_SEARCH_FETCH_PORT`、`DEEPSEEK_DEFAULT_LANG`
- 配置文件:`deepseek_api_key`、`deepseek_api_url`、`server_host`、`server_port`、`request_timeout`、`fetch_timeout`、`cache_ttl`、`cache_max_bytes`、`default_lang`、`rate_limit_rps`、`rate_limit_burst`、`auth_fail_limit`、`auth_fail_window`、`auth_fail_block`、`ip_rate_limit_rps`、`ip_rate_limit_burst`、`trusted_proxies`、`http_max_idle_conns`、`http_max_idle_conns_per_host`、`http_idle_conn_timeout`、`http_tls_handshake_timeout`、`reset_connection_on_error`
- 数据目录默认 `~/.deepseek_search_fetch`,可用 `--data-dir` 或 `DEEPSEEK_SEARCH_FETCH_HOME` 覆盖(优先级:启动参数 > 环境变量 > 默认);配置、缓存、访问密钥、日志落在同一目录;以服务/容器等非交互账户运行时必须显式指定
- `fetch_timeout` 只作用于抓取请求(浏览器渲染更慢),搜索仍使用 `request_timeout`
- `cache_ttl` 是缓存条目有效期,默认 `30m`;`cache_max_bytes` 是页级活跃大小上限,默认 `1073741824`(1 GiB),不代表磁盘占用
- `default_lang` 控制搜索的默认语言补全:空字符串关闭(默认),`auto` 读取 `LC_ALL`/`LC_MESSAGES`/`LANG`,或写两位语言码(如 `zh`);只作用于 `web_ext_search` 与 CLI `search`,query 已含 `lang:` 时不改动;原生 Windows 通常没有这些环境变量,`auto` 会关闭,建议显式配置语言码
- 鉴权/限流参数只从配置文件读取,不提供环境变量或启动参数:`rate_limit_rps`/`rate_limit_burst` 控制每密钥令牌桶(默认 10 rps / burst 20,`0` 关闭),`auth_fail_limit`/`auth_fail_window`/`auth_fail_block` 控制每 IP 鉴权失败封禁(默认 10 次/分钟封 5 分钟),`ip_rate_limit_rps`/`ip_rate_limit_burst` 控制每 IP 全量请求限流(默认关闭),`trusted_proxies` 是可信代理 CIDR 列表
- 缓存文件:`~/.deepseek_search_fetch/cache.db`(SQLite,WAL 模式,权限 0600),可用 `flushall` 清空
- 访问密钥库:`~/.deepseek_search_fetch/auth.db`(SQLite,WAL 模式,权限 0600),只存 SHA-256 哈希与首尾片段;`flushall` 不清空;没有密钥时 `/mcp` 不鉴权
- `reset_connection_on_error` 为 true 时,任何上游调用失败都会丢弃空闲连接,下次调用重新建连
- 日志目录:`~/.deepseek_search_fetch/logs/`,文件 `server.log`
- 鉴权/限流拒绝事件写入 `server.log`,包含 `event`、`remote`、`peer`、`method`、`path`、`status`、`reason` 等稳定字段,供 fail2ban 匹配;不记录 `Authorization`、`X-API-Key`、完整密钥或 query。配合示例见 `docs/deployment/fail2ban.md`。
- 滚动策略:单文件 10 MiB,最多保留 5 个文件(当前 `server.log` + 4 个备份);`server` 模式同时输出到 stderr,CLI 模式只写日志文件,保证 stdout 只有 JSON

## 构建与测试

需要 Go 1.25 或更高版本。

- 构建:`go build ./...`
- 运行 MCP 服务:`go run ./cmd/deepseek-search-fetch-mcp server`(默认监听 `127.0.0.1:18081`;不带子命令会报错并打印用法)
- 静态检查:`go vet ./...`
- 格式化检查:`gofmt -l .`
- 单元/集成测试:`go test ./...`
- 竞态检查(需要 CGO 与 C 编译器):`CGO_ENABLED=1 go test -race ./...`
- 构建镜像:`docker build -t deepseek-search-fetch-mcp .`

## 命令行

子命令必须显式指定,无参数直接报错:

- `server [--host 127.0.0.1] [--port 18081]`:启动 MCP 服务。
- `search [--count 10] [--content] <query>`:直接调用上游搜索,输出 JSON。默认返回 `summary`,不返回正文;`--content` 额外返回 `content`。结果会写入缓存。
- `fetch [--summary] [--refresh] <url>`:直接调用抓取流程,输出 JSON,结果会写入缓存。CLI 不分页,直接返回完整正文;`--refresh` 强制跳过缓存。分页仅用于 MCP agent。
- `flushall`:清空 SQLite 缓存。
- `apikey add <name>`:生成访问密钥,完整值只显示一次,写入 `auth.db`;不需要 DeepSeek API Key。
- `apikey list`:列出访问密钥,只显示首尾脱敏(`dsf_Ab12Cd34…x9Yz`)、创建时间与最后使用时间。
- `apikey remove <name>`:硬删除访问密钥;删除最后一个密钥后 `/mcp` 回到不鉴权模式。
- `--help`:查看顶层用法;各子命令也支持 `--help`。

CLI 模式日志只写入日志文件,stdout 只输出 JSON;错误信息写 stderr。全局参数 `--deepseek-api-key`、`--deepseek-api-url`、`--data-dir` 写在子命令之后。

版本与构建渠道:

- 本地 `go build` 默认 `version=dev`、`build=local`
- 本地指定版本:`-ldflags "-X main.version=v1.2.3"`,渠道仍为 `local`
- Docker/GHCR:注入 `build=ghcr`,版本取构建参数 `VERSION`
- MCP 握手信息中的版本为 `version+build`,例如 `dev+local`、`v1.2.3+ghcr`

国内网络下拉取依赖需先设置 `GOPROXY=https://goproxy.cn,direct`。

测试以 MCP 协议边界为主,缓存、鉴权/限流与命令行逻辑另有独立单元测试;上游全部使用 `httptest` 假服务,不访问真实网络。真实联调时直接注入环境变量或写入固定配置文件,不要把 Key 提交到仓库。

## 目录结构

internal 分三层:

- `internal/mcp`:MCP 层。装配工具、Streamable HTTP 传输、健康检查。
- `internal/openapi`:OpenAPI 工具层。DeepSeek Search/Fetch 客户端、工具定义、参数校验、SQLite 缓存。
- `internal/auth`:鉴权层。访问密钥 SQLite 存储、鉴权/限流中间件、客户端 IP 解析。
- `cmd/deepseek-search-fetch-mcp`:进程入口、配置解析、子命令(server/search/fetch/flushall/apikey)、滚动日志、信号处理与优雅关闭。

## Agent skills

### 工单系统

工单与规格记录在 GitHub Issues(`origin` 仓库),通过 `gh` CLI 读写。详见 `docs/agents/issue-tracker.md`。

### Triage 标签

沿用五个默认标签:`needs-triage`、`needs-info`、`ready-for-agent`、`ready-for-human`、`wontfix`。详见 `docs/agents/triage-labels.md`。

### 域文档

单上下文布局:根目录 `CONTEXT.md` 加 `docs/adr/`。详见 `docs/agents/domain.md`。
