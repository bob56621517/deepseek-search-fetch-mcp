# ADR-0001: 使用 SQLite 缓存搜索正文与摘要

- 状态:已接受
- 日期:2026-10-23
- 后续:容量统计与淘汰策略已由 [ADR-0002](0002-page-level-cache-capacity.md) 取代

## 背景

实测上游 `search` 的 `summary` 并非文档声称的 600–800 字符,而是通常 1–6k 字符;`need_content=true` 时返回的 `content` 与 `fetch` 的 `content` 相似度在 0.99 以上,单条正文最长观察到 141k 字符。若把 `summary` 直接放进搜索结果,一次 `count=10` 就能产生约 45–48 KB 输出;`count=50` 超过 200 KB,存在撑爆上下文的实际风险。

同时,`fetch` 是独立计费请求,且浏览器渲染常见 3–20 秒。分页阅读、多个 subagent 重复抓取同一 URL 都会放大延迟与费用。

## 决策

1. **搜索结果只返回 `snippet`**,不再暴露 `summary` 参数或摘要字段。`snippet` 原样输出,不做截断。
2. **搜索固定请求 `need_content=true`**,把每条结果的 `title`、`content`、`summary` 写入缓存。一次搜索只计一次上游请求,缓存预热不额外计费。
3. **缓存使用 SQLite**,文件固定为 `~/.deepseek_search_fetch/cache.db`,以规范化 URL 为键,正文与摘要分别记录时间戳。
   - 驱动使用纯 Go 的 `modernc.org/sqlite`,保持 `CGO_ENABLED=0` 构建。
   - TTL 默认 30 分钟,容量默认 1 GiB,均可通过 `config.json` 配置。
   - 开启 WAL、`busy_timeout`,单连接串行访问,避免本地锁竞争。
4. **`web_ext_fetch` 缓存优先**:
   - `summary=true`:命中摘要则直接返回,不分页;没有摘要时回退为正文。
   - `summary=false`:命中正文则按 `offset`/`limit` 分页返回;未命中才请求上游。
   - `refresh=true`:跳过缓存读取并强制刷新,同时丢弃旧摘要。
   - 响应统一包含 `contentLength`、`offset`、`length`、`hasSummary`;正文模式额外包含 `hasMore`、`nextOffset`。
5. **MCP 工具的正文按 rune 分页**,默认 8000、上限 16000,避免切断 UTF-8 并控制上下文;CLI `fetch` 不分页,一次性返回完整正文。
6. **CLI 复用同一套缓存与流程**:`search`(默认返回 summary,`--content` 额外返回正文)、`fetch`(完整正文)、`flushall`。
7. **缓存失败不影响主功能**:写入或读取失败只记日志,搜索与抓取仍正常返回。

## 后果

- 依赖体积与构建时间增加(`modernc.org/sqlite` 及其间接依赖),但 Docker 构建不需要 CGO。
- 网页正文会落盘,需依赖 TTL 清理、容量淘汰与 0600 文件权限控制残留。
- 摘要以 URL 为键缓存,可能来自此前的某次搜索;响应通过 `summaryQuery` 标明来源,不保证与当前问题精确对应。
- 缓存条目在 TTL 内被淘汰时,后续分页会重新抓取,`offset` 可能发生偏移;30 分钟 TTL 下概率很低。
- SQLite 让缓存容量明显大于内存,并在进程重启后保留,这是选择它而不是内存 LRU 的主要原因。

## 备选方案

- **内存 LRU**:实现更简单,但容量受内存限制,重启即丢失。
- **bbolt**:同样是纯 Go 持久化 KV,依赖更轻,但缺少 SQL 的 TTL/淘汰表达力。
- **不缓存,只分页**:实现最简单,但分页会重复请求上游并重复计费。
