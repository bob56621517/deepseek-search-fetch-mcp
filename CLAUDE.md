# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 语言约定

- 文档、代码注释、提交信息、PR/MR 描述使用中文。
- 由英文模板生成的文件(README 模板、Issue/PR 模板、注释模板等)落地前翻译为中文。
- 仅当符号含义强于语言含义时保留原文:标识符、关键字、协议与格式名、配置键、命令、URL、module path(如 `MCP`、Streamable HTTP、`ghcr.io`、`github.com/bob56621517/deepseek-search-fetch-mcp`)。

## 项目约定

- Go 项目,module 为 `github.com/bob56621517/deepseek-search-fetch-mcp`。
- 同一可执行文件有两种形态:MCP server(`server` 子命令)与 CLI(`search` / `fetch` / `flushall`)。
- MCP server 使用官方 Go SDK `github.com/modelcontextprotocol/go-sdk`,以 Streamable HTTP 形式暴露。
- 只暴露 DeepSeek 官方的两个工具:`web_ext_search`、`web_ext_fetch`。
- `web_ext_search` 只返回 `snippet`,没有 `summary` 参数;搜索时固定向上游请求 `need_content=true`,把每条结果的 `content`、`summary` 写入 SQLite 缓存。
- 搜索 `count` 默认 10;低于 5 自动按 5 处理;超过 50 报错。
- `web_ext_fetch` 缓存优先,支持 `summary`(读取搜索缓存的摘要,不分页)、`offset`/`limit`(正文按 rune 分页)与 `refresh`(跳过缓存强制刷新)。
- 缓存使用 SQLite,文件为 `~/.deepseek_search_fetch/cache.db`;TTL 默认 30 分钟、容量默认 1 GiB,均可配置。缓存故障只记日志,不影响主功能。
- 接口契约以 `api/openapi.yaml` 为准;缓存与 CLI 属于包装层行为,不改变上游契约。
- 唯一凭据是 DeepSeek API Key。
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

固定配置文件:`~/.deepseek_search_fetch/config.json`

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
  "http_max_idle_conns": 5,
  "http_max_idle_conns_per_host": 5,
  "http_idle_conn_timeout": "90s",
  "http_tls_handshake_timeout": "10s",
  "reset_connection_on_error": true
}
```

首次启动时如果目录或文件不存在,会自动创建(Key 为空)。配置只在启动时读取一次,修改后需要重启。配置优先级:**启动参数 > 环境变量 > 配置文件 > 默认值**。

- 启动参数:`--deepseek-api-key`、`--deepseek-api-url`、`--host`、`--port`
- 环境变量:`DEEPSEEK_API_KEY`、`DEEPSEEK_API_URL`、`DEEPSEEK_SEARCH_FETCH_HOST`、`DEEPSEEK_SEARCH_FETCH_PORT`
- 配置文件:`deepseek_api_key`、`deepseek_api_url`、`server_host`、`server_port`、`request_timeout`、`fetch_timeout`、`cache_ttl`、`cache_max_bytes`、`http_max_idle_conns`、`http_max_idle_conns_per_host`、`http_idle_conn_timeout`、`http_tls_handshake_timeout`、`reset_connection_on_error`
- `fetch_timeout` 只作用于抓取请求(浏览器渲染更慢),搜索仍使用 `request_timeout`
- `cache_ttl` 是缓存条目有效期,默认 `30m`;`cache_max_bytes` 是缓存容量上限,默认 `1073741824`(1 GiB)
- 缓存文件:`~/.deepseek_search_fetch/cache.db`(SQLite,WAL 模式,权限 0600),可用 `flushall` 清空
- `reset_connection_on_error` 为 true 时,任何上游调用失败都会丢弃空闲连接,下次调用重新建连
- 日志目录:`~/.deepseek_search_fetch/logs/`,文件 `server.log`
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
- `--help`:查看顶层用法;各子命令也支持 `--help`。

CLI 模式日志只写入日志文件,stdout 只输出 JSON;错误信息写 stderr。全局参数 `--deepseek-api-key`、`--deepseek-api-url` 写在子命令之后。

版本与构建渠道:

- 本地 `go build` 默认 `version=dev`、`build=local`
- 本地指定版本:`-ldflags "-X main.version=v1.2.3"`,渠道仍为 `local`
- Docker/GHCR:注入 `build=ghcr`,版本取构建参数 `VERSION`
- MCP 握手信息中的版本为 `version+build`,例如 `dev+local`、`v1.2.3+ghcr`

国内网络下拉取依赖需先设置 `GOPROXY=https://goproxy.cn,direct`。

测试以 MCP 协议边界为主,缓存与命令行逻辑另有独立单元测试;上游全部使用 `httptest` 假服务,不访问真实网络。真实联调时直接注入环境变量或写入固定配置文件,不要把 Key 提交到仓库。

## 目录结构

代码只分两层:

- `internal/mcp`:MCP 层。装配工具、Streamable HTTP 传输、健康检查。
- `internal/openapi`:OpenAPI 工具层。DeepSeek Search/Fetch 客户端、工具定义、参数校验、SQLite 缓存。
- `cmd/deepseek-search-fetch-mcp`:进程入口、配置解析、子命令(server/search/fetch/flushall)、滚动日志、信号处理与优雅关闭。

## Agent skills

### 工单系统

工单与规格记录在 GitHub Issues(`origin` 仓库),通过 `gh` CLI 读写。详见 `docs/agents/issue-tracker.md`。

### Triage 标签

沿用五个默认标签:`needs-triage`、`needs-info`、`ready-for-agent`、`ready-for-human`、`wontfix`。详见 `docs/agents/triage-labels.md`。

### 域文档

单上下文布局:根目录 `CONTEXT.md` 加 `docs/adr/`。详见 `docs/agents/domain.md`。
