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
同一可执行文件的直接调用形态,包含 `server`、`search`、`fetch`、`flushall`、`apikey` 子命令。CLI 模式 stdout 只输出 JSON,日志只写日志文件。
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

**访问密钥 (Access key)**:
MCP 客户端访问本服务的凭据,保存在数据目录下的 `auth.db`,与上游 `deepseek_api_key` 无关。只保存 SHA-256 哈希与首尾片段;完整值只在 `apikey add` 时显示一次。
_Avoid_: 上游 API Key、token、密码

**鉴权模式 (Auth mode)**:
由 `api_keys` 是否存在决定。没有密钥时不鉴权;至少一个密钥时 `/mcp` 强制校验。删除最后一个密钥后立即回到不鉴权模式。
_Avoid_: 登录、会话

**限流 (Rate limit)**:
进程内的每密钥令牌桶与每 IP 鉴权失败封禁。默认每密钥 10 rps / burst 20;同 IP 10 次失败/分钟封 5 分钟;每 IP 全量限流默认关闭。
_Avoid_: 配额、计费

**审计日志 (Audit log)**:
写入 `server.log` 的拒绝事件,包含 `event`、`remote`、`status`、`reason` 等稳定字段,供 fail2ban 匹配;不记录任何密钥。
_Avoid_: 访问日志、错误日志

**错误体 (Error envelope)**:
上游错误响应,OpenAI 兼容格式:`{"error":{"message","type","param","code"}}`。
_Avoid_: 错误码、异常

**配置文件 (Config file)**:
默认路径 `~/.deepseek_search_fetch/config.json`(数据目录可配置),承载 `deepseek_api_key`、`deepseek_api_url`、`server_host`、`server_port`、超时参数、缓存参数(`cache_ttl`、`cache_max_bytes`)、默认语言(`default_lang`)以及鉴权/限流参数(`rate_limit_*`、`auth_fail_*`、`ip_rate_limit_*`、`trusted_proxies`)。首次启动时自动创建。访问密钥本身不在配置文件中,由 `apikey` 命令管理。
_Avoid_: 设置、环境

## 运行时

**MCP 层**:
负责协议与传输的薄层,对应 `internal/mcp`。不感知具体上游接口。
_Avoid_: 服务层、接入层

**OpenAPI 工具层**:
负责工具定义、参数校验与上游调用,对应 `internal/openapi`。不感知 MCP 传输。
_Avoid_: 业务层、适配层

**鉴权层**:
负责访问密钥存储、鉴权/限流中间件与客户端 IP 解析,对应 `internal/auth`。只处理 `/mcp` 入口,不感知具体工具与上游接口。
_Avoid_: 认证服务、权限系统

**连接复位 (Connection reset)**:
上游调用失败后丢弃空闲连接,下次调用重新建连,避免复用处于异常状态的连接。
_Avoid_: 重连、断线重试
