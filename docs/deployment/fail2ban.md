# fail2ban 配合

服务把鉴权与限流的拒绝事件写入 `<data_dir>/logs/server.log`。fail2ban 通过匹配日志中的 `event`、`remote`、`status` 字段封禁客户端 IP。

## 事件约定

| event | status | 含义 | 是否建议封禁 |
|---|---|---|---|
| `auth_fail` | 401 | 缺少、无效或冲突的访问密钥 | 是 |
| `auth_blocked` | 429 | 同一 IP 鉴权失败次数超限 | 是 |
| `key_rate_limited` | 429 | 合法密钥触发每 key 限流 | 否 |
| `ip_rate_limited` | 429 | 同一 IP 触发全量请求限流 | 可选 |
| `auth_unavailable` | 503 | 鉴权库不可用 | 否 |

不要按所有 429 封禁：合法客户端被限流时也会返回 429，误封会影响正常使用。

日志行示例：

```text
time=2026-11-06T10:00:00.000+08:00 level=WARN msg=请求被拒绝 event=auth_fail remote=203.0.113.5 peer=127.0.0.1 method=POST path=/mcp status=401 reason=invalid_key
```

- `remote` 是解析后的客户端地址；`peer` 是直接 TCP 对端。
- 日志不包含 `Authorization`、`X-API-Key`、完整密钥或 query。
- 服务端自身的失败封禁默认是 5 分钟，fail2ban 的 `bantime` 可以更长（例如 1 小时），两者互补。

## filter

创建 `/etc/fail2ban/filter.d/deepseek-search-fetch.conf`：

```ini
[Definition]
failregex = ^.*\bevent=auth_fail\b.*\bremote=<HOST>\b.*\bstatus=401\b.*$
            ^.*\bevent=auth_blocked\b.*\bremote=<HOST>\b.*\bstatus=429\b.*$
ignoreregex =
```

如果 fail2ban 无法识别 `time=` 字段，可在 `[Definition]` 中补一行：

```ini
datepattern = ^time=%Y-%m-%dT%H:%M:%S
```

`datepattern` 不消费时区偏移，fail2ban 会按所在主机时区解释时间。日志时区与 fail2ban 主机不一致时，所有行会整体偏移，`findtime` 的相对关系通常仍成立；最简单的是让两边保持同一时区，并用下面的 `fail2ban-regex` 实测。

用真实日志验证规则：

```bash
fail2ban-regex /home/<user>/.deepseek_search_fetch/logs/server.log \
  /etc/fail2ban/filter.d/deepseek-search-fetch.conf
```

## jail

创建 `/etc/fail2ban/jail.d/deepseek-search-fetch.local`：

```ini
[deepseek-search-fetch]
enabled = true
filter = deepseek-search-fetch
logpath = /home/<user>/.deepseek_search_fetch/logs/server.log
backend = auto
maxretry = 10
findtime = 10m
bantime = 1h
ignoreip = 127.0.0.1/8 ::1
```

- `maxretry` / `findtime` 可以与配置中的 `auth_fail_limit` / `auth_fail_window` 对齐或更宽松。
- `ignoreip` 建议加入本机、内网监控与反向代理地址。
- 修改后执行 `fail2ban-client reload`。

## 反向代理与真实 IP

如果服务前面有 nginx、Caddy 等反向代理，必须在配置文件里设置 `trusted_proxies`：

```json
{
  "trusted_proxies": ["127.0.0.1"]
}
```

- `trusted_proxies` **只能填写反向代理自身的地址或最小 CIDR**，不要填客户端网段。若大网段内同时存在直连客户端，它们可以伪造 `X-Forwarded-For`，绕过每 IP 限流与失败封禁，甚至诱导 fail2ban 误封第三方地址。
- 反向代理应**追加**而不是无条件透传客户端自带的 `X-Forwarded-For`。nginx 常用 `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`。
- 未配置时 `remote` 是代理 IP，fail2ban 会封错对象。
- `X-Forwarded-For` 从右往左解析：最右侧非法时回退到直接对端；左侧出现非法项时会截断并保留右侧已解析出的地址，避免把失败计数错误地记到代理 IP。
- 建议把代理地址加入 jail 的 `ignoreip`，避免代理自身因其他原因被误封。

## 日志轮转

服务日志单文件 10 MiB，保留 5 个文件。`backend = auto` 通常可以跟随轮转；如果 fail2ban 漏读，可改为：

```ini
backend = polling
```

## Docker

容器内日志同时写入 `stderr` 和 `<data_dir>/logs/server.log`。建议把数据目录 bind mount 到宿主机固定路径：

```bash
docker run -d --name deepseek-search-fetch-mcp \
  -p 18081:18081 \
  -e DEEPSEEK_API_KEY=sk-... \
  -v /srv/deepseek-sf:/data \
  deepseek-search-fetch-mcp
```

则宿主机日志路径为 `/srv/deepseek-sf/.deepseek_search_fetch/logs/server.log`。也可以让 fail2ban 读反向代理的访问日志，但 `event` 字段只存在于服务日志中。

## Windows 限制

fail2ban 是 Linux 工具，不能直接管理 Windows 防火墙。Windows 上可选：

- 在 Linux 反向代理或跳板机上运行 fail2ban，服务只暴露给该代理；
- 使用 WSL 运行 fail2ban，但它无法直接封禁 Windows 主机端口，不建议单独使用；
- 用 PowerShell 计划任务解析同一份 `server.log`，再调用 `New-NetFirewallRule` 或 `netsh advfirewall` 封禁。

日志字段设计对以上方案都通用。

## 安全提醒

- 访问密钥只解决身份校验，不提供传输加密；公网部署必须放在 TLS 反代后面。
- 只封 `auth_fail` 与 `auth_blocked`，不要按所有 429 或 503 封禁。
- 定期检查 `apikey list` 中的 `last_used_at`，清理不再使用的密钥。
