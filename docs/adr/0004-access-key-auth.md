# ADR-0004: 客户端访问密钥鉴权、限流与 fail2ban 配合

- 状态:已接受
- 日期:2026-11-06
- 关联:#9
- 规格:#9 正文

## 背景

MCP server 默认只监听 `127.0.0.1`,此前 `/mcp` 没有任何鉴权。部署到局域网、公网或多人共享环境后,需要区分客户端、限制滥用,并让拒绝事件可被 fail2ban 消费。

本 ADR 中的**访问密钥**是 MCP 客户端访问本服务的凭据,与上游的 `deepseek_api_key` 无关。`server`/`search`/`fetch` 仍然需要上游 Key;访问密钥只保护 MCP 入口。

## 决策

1. **独立 SQLite 存储,只存哈希**
   - 使用 `<data-dir>/auth.db`,不放进 `cache.db`;`flushall` 只清缓存,不影响访问密钥。
   - 表 `api_keys(name, key_hash, key_prefix, key_suffix, created_at, last_used_at)`。
   - `key_hash` 为 SHA-256 hex;`key_prefix` 取前 12 个字符,`key_suffix` 取后 4 个字符,仅用于首尾脱敏展示。
   - `apikey add` 生成 `dsf_` + 32 字节 `crypto/rand` 的 base64url,完整 key 只显示一次。
   - `auth.db` 打开失败时 server 启动失败,不像缓存那样降级。

2. **CLI 使用 `apikey` 子命令组**
   - `apikey add <name>`、`apikey list`、`apikey remove <name>`。
   - 三个命令都不需要 DeepSeek API Key,支持 `--data-dir`,stdout 只输出 JSON。
   - `add` 是唯一显示完整 key 的入口;`list` 显示 `dsf_Ab12Cd34…x9Yz` 形式的首尾脱敏。
   - `remove` 硬删除;删除最后一个 key 后输出 `auth=disabled` 并警告。

3. **按 key 数量动态切换鉴权模式**
   - 每个 `/mcp` 请求检查是否存在至少一个 key。
   - 0 个 key:不鉴权,静默提供服务,不拒绝请求。
   - ≥1 个 key:强校验,所有 `/mcp` 请求必须携带有效访问密钥。
   - 不引入 `require_auth`;是否启用只看 key 是否存在。
   - `/health`、`/healthz` 永远豁免;鉴权库不可用返回 503 `auth_unavailable`。

4. **HTTP 凭据与状态码**
   - 接受 `Authorization: Bearer <key>` 和 `X-API-Key: <key>`;两者冲突时 401。
   - 401:缺失/无效/冲突凭据,带 `WWW-Authenticate: Bearer`。
   - 429:限流或鉴权失败次数超限,带 `Retry-After`。
   - 503:鉴权存储不可用。
   - 不使用 query 参数传递 key;key 长度上限 512 字节。

5. **限流**
   - 每 key 内存令牌桶,默认 10 rps / burst 20;`rate_limit_rps=0` 关闭。
   - 每 IP 鉴权失败计数,默认 10 次/60 秒,超限 429 并封 5 分钟;成功一次清零。
   - 每 IP 全量请求限流默认关闭,公网部署可开启。
   - 单进程内存实现;多实例各自限流。

6. **fail2ban 事件**
   - 拒绝日志包含稳定的 `event`、`remote`、`peer`、`method`、`path`、`status`、`reason` 字段。
   - 推荐 filter 只匹配 `auth_fail`(401)与 `auth_blocked`(429),不匹配普通 `key_rate_limited`(429),避免误封合法客户端。
   - 日志绝不记录 `Authorization`、`X-API-Key`、完整 key 或 query。
   - 通过 `trusted_proxies` CIDR 列表决定是否信任 `X-Forwarded-For`;未配置时使用 `RemoteAddr`。

7. **代码结构**
   - 新增 `internal/auth`:SQLite store、鉴权/限流中间件、客户端 IP 解析。
   - `internal/mcp` 负责装配;`cmd/deepseek-search-fetch-mcp` 增加 `apikey` 子命令。
   - `api/openapi.yaml` 不变:鉴权与限流是服务端包装层行为,不改变上游契约。

## 后果

- 只存哈希意味着无法找回旧 key;泄露或丢失时执行 `apikey remove` + `apikey add` 轮换。
- 没有 key 时服务仍然开放。部署者必须通过 `apikey add` 主动启用;启动日志会记录 `mode=disabled`,监听非 loopback 时给出 WARN。
- 删除最后一个 key 会立即回到开放模式,这是“以 key 存在为开关”的直接后果。
- 限流是单进程内存态;多副本部署时每个副本独立计数,需要网关层或共享存储才能统一限流。
- fail2ban 依赖稳定日志字段和正确的代理信任配置。Windows 上 fail2ban 不能直接管理 Windows 防火墙,需要 WSL/Linux 反代或替代方案。
- `X-Forwarded-For` 从右往左解析;最右侧非法时回退到直接对端,左侧出现非法项时会截断并保留右侧已解析地址,避免把失败计数错误地记到代理 IP。
- 数据目录无法确定时,server 会因无法打开 `auth.db` 而启动失败;服务/容器等非交互账户必须显式指定 `--data-dir` 或 `DEEPSEEK_SEARCH_FETCH_HOME`。这与旧版“缓存不可用仍继续运行”的行为不同。
- 访问密钥不等于传输加密;公网部署仍需 TLS 反代,否则 Bearer key 会明文暴露。
- `auth.db` 只含哈希、首尾片段、名称与时间戳,备份风险低。

## 备选方案

- **明文存储 key**:可以在 `list` 中恢复完整 key,但数据库泄露即全部凭据泄露;拒绝。
- **复用 `cache.db`**:少一个文件,但 `flushall`、`VACUUM`、缓存降级会与鉴权耦合;拒绝。
- **顶层裸 `add/list/remove`**:命令更短,但命名空间冲突风险高;拒绝。
- **fail2ban 按 429 全量封禁**:会把合法客户端的限流误判为攻击;拒绝。
- **`require_auth` 开关**:更安全,但需求明确要求“没有 key 时静默提供服务”;不实现。
- **OAuth/JWT/mTLS**:能力更强,但超出当前单用户/小团队场景;不实现。
