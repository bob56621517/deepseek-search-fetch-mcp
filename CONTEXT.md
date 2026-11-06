# deepseek-search-fetch-mcp

把 DeepSeek 官方的网页搜索与抓取能力暴露为单一 MCP 服务。项目自身不实现检索逻辑,只负责 MCP 协议、工具描述、参数校验与凭据注入。

## Language

## 能力与工具

**Search (搜索)**:
按 query 检索网页,返回结构化结果列表。对应上游 `POST /v1/tools/search`。只返回 `snippet`,不返回 `summary`、`content` 与 `site_icon`。搜索时会向上游请求 `need_content=true`,把每条结果的正文与摘要写入缓存,供后续 Fetch 使用。配置 `default_lang` 后,query 未写 `lang:` 时会自动补全默认语言。
_Avoid_: 检索、查询接口

**Fetch (抓取)**:
抓取单个网页,返回页面标题与 Markdown 正文。对应上游 `POST /v1/tools/fetch`。默认缓存优先;`summary=true` 时优先返回搜索缓存的摘要,没有摘要则回退为正文;MCP 工具的正文通过 `offset`/`limit` 按 rune 分页返回,CLI 则一次性返回完整正文;`refresh=true` 跳过缓存并刷新。
_Avoid_: 下载、爬取

**缓存 (Cache)**:
进程间共享的 SQLite 缓存,默认文件 `~/.deepseek_search_fetch/cache.db`(数据目录可用 `--data-dir`/`DEEPSEEK_SEARCH_FETCH_HOME` 覆盖)。以规范化 URL 为键,分别记录正文与摘要及各自时间戳;TTL 与容量可配置,容量按页级活跃大小判断(不是磁盘占用)。缓存故障只记日志,不影响搜索与抓取主流程。
_Avoid_: 存储、数据库

**命令行 (CLI)**:
同一可执行文件的直接调用形态,包含 `server`、`search`、`fetch`、`flushall` 四个子命令。CLI 模式 stdout 只输出 JSON,日志只写日志文件。
_Avoid_: 脚本、工具集

**工具 (Tool)**:
MCP 客户端可调用的一项能力。当前固定为 `web_ext_search` 与 `web_ext_fetch`;`web_ext_search` 是扩展搜索,支持站点、语言、时间范围等过滤;`web_ext_fetch` 同时承担缓存摘要读取与正文分页。
_Avoid_: 函数、接口、API

**上游 (Upstream)**:
DeepSeek 官方 API,Base URL 为 `https://api.deepseek.com`。
_Avoid_: 供应商、渠道、backend

**接口契约 (API contract)**:
`api/openapi.yaml`,定义两个工具的请求与响应结构。代码修改应与之一致。
_Avoid_: 文档、spec 文件

## 调用面

**会话 (Session)**:
stateless 模式下单次 HTTP 请求对应的临时 MCP 会话,不跨请求保留状态。
_Avoid_: 连接、上下文

**错误体 (Error envelope)**:
上游错误响应,OpenAI 兼容格式:`{"error":{"message","type","param","code"}}`。
_Avoid_: 错误码、异常

**配置文件 (Config file)**:
默认路径 `~/.deepseek_search_fetch/config.json`(数据目录可配置),承载 `deepseek_api_key`、`deepseek_api_url`、`server_host`、`server_port`、超时参数、缓存参数(`cache_ttl`、`cache_max_bytes`)与默认语言(`default_lang`)。首次启动时自动创建。
_Avoid_: 设置、环境

## 运行时

**MCP 层**:
负责协议与传输的薄层,对应 `internal/mcp`。不感知具体上游接口。
_Avoid_: 服务层、接入层

**OpenAPI 工具层**:
负责工具定义、参数校验与上游调用,对应 `internal/openapi`。不感知 MCP 传输。
_Avoid_: 业务层、适配层

**连接复位 (Connection reset)**:
上游调用失败后丢弃空闲连接,下次调用重新建连,避免复用处于异常状态的连接。
_Avoid_: 重连、断线重试
