# ADR-0002: 用页级活跃大小统计缓存容量并批量 LRU 淘汰

- 状态:已接受
- 日期:2026-11-06

## 背景

ADR-0001 引入 SQLite 缓存时,容量判断基于 `SUM(size_bytes)`:

- `size_bytes` 没有任何索引覆盖,查询会扫描整张表,并读取 `content`、`summary` 等大文本页;缓存接近 1 GiB 时成本明显。
- 它只统计业务字段,不含索引、SQLite 页开销、`url`、`summary_query` 与 WAL,`cache_max_bytes` 只是一个偏小的逻辑上限。
- 超限后的淘汰会流式读取所有行来收集 victim,最坏情况下同样接近全表扫描。

本地缓存只需要模糊、稳定、低开销的容量控制,不需要精确到字段字节。

## 决策

1. **容量指标改为页级活跃大小**:`(page_count - freelist_count) * page_size`,单条查询、O(1),包含索引与页开销。删除后 freelist 增加,指标立即下降。
2. **不做 WAL checkpoint**。实测同一连接提交后 `page_count`/`freelist_count` 已反映 WAL 中的已提交变更,checkpoint 前后指标不变,只增加 I/O 与失败模式。
3. **超限后按 `last_used` 批量淘汰**:
   - `DELETE FROM web_cache WHERE url IN (SELECT url FROM web_cache ORDER BY last_used, url LIMIT ?)`;
   - 起始批量 16 行,每轮翻倍至 4096,每批使用独立隐式事务,提交后重新测量;
   - 目标水位为 `cache_max_bytes` 的 95%;
   - 最多 32 轮;无行可删或连续两轮指标不下降时提前停止(页级回收可能腾不出整页)。
4. **索引调整**:新建复合索引 `(last_used, url)` 覆盖淘汰排序;删除旧的 `last_used` 单列索引与冗余的 `summary_at` 索引;保留 `content_at` 索引供 TTL 范围扫描。
   - 使用新索引名而不是原地修改,避免 `CREATE INDEX IF NOT EXISTS` 遇到同名索引时静默保留旧定义。
   - 所有 DDL 幂等,不引入 `PRAGMA user_version`。
5. **保留 `size_bytes` 列与写入逻辑**,但不再参与容量判断。删列需要迁移、多进程互斥与降级保护,收益不足以抵消风险。

## 后果

- `cache_max_bytes` 变为**逻辑活跃页上限,不是磁盘占用上限**:`cache.db` 与 `cache.db-wal` 可能更大,freelist 页仍占文件空间;只有 `flushall` 的 VACUUM 会回收文件。
- 删除粒度是页:删掉少量小行可能不会立即降低指标,这是预期行为,不是故障。
- 旧库首次启动需要建立 `(last_used, url)` 复合索引,一次性扫描全表;1 GiB 缓存下可能不是瞬间完成。
- `size_bytes` 成为历史字段,文档标注其不参与容量判断。
- 单连接(`SetMaxOpenConns(1)`)下,测量不得在任何未提交事务内进行,否则会永久阻塞。

## 备选方案

- **保留 `SUM(size_bytes)`**:精确到字段,但全表扫描且不含索引/页开销,不满足低开销目标。
- **用文件大小触发**:WAL 未 checkpoint 会低估;DELETE 不缩文件,会导致淘汰后指标不降、反复清空缓存。
- **`auto_vacuum=INCREMENTAL`**:能在删除后回收文件空间,但需建库时开启或执行迁移,收益与本议题无关。
- **触发器维护计数器**:每次写入多一次状态表写入,且仍只统计业务字段,解决不了页开销问题。
- **删除 `size_bytes` 列并迁移**:schema 更干净,但需要幂等迁移、多进程互斥与降级保护,风险明显更大。
