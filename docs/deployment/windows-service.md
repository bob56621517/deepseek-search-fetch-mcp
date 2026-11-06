# Windows 服务部署

本文说明如何用 [NSSM](https://nssm.cc/) 把 `deepseek-search-fetch-mcp server` 注册为 Windows 开机自启服务。

## 背景：为什么需要显式数据目录

程序的数据目录（配置、缓存、访问密钥、日志）默认是 `os.UserHomeDir()/.deepseek_search_fetch`。Windows 服务默认以 `LocalSystem` 账户运行，其「家目录」指向 `C:\Windows\system32\config\systemprofile`，与登录用户目录不同，会导致：

- 读不到用户目录下已有的 `config.json`，启动直接因「未配置 DeepSeek API Key」失败；
- 即使用环境变量补上 Key，缓存、访问密钥与日志也会落到 SYSTEM 私有目录，与 CLI 各用一份，排查时「明明搜过却不在缓存里」。

因此以服务方式运行时，必须用 `DEEPSEEK_SEARCH_FETCH_HOME` 或 `--data-dir` 显式指定数据目录。

> 早期版本不支持数据目录配置，只能靠注入 `USERPROFILE` 覆盖家目录（依赖 Go 对 `%USERPROFILE%` 的解析细节）。现在已支持 `--data-dir` / `DEEPSEEK_SEARCH_FETCH_HOME`，不要再使用 `USERPROFILE` 覆盖。

## 前置条件

- 已通过 `go install` 或源码构建得到 `deepseek-search-fetch-mcp.exe`
- 已安装 NSSM（`choco install nssm`，或从官网下载 `nssm.exe` 并加入 `PATH`）
- 以管理员身份打开 PowerShell（注册服务需要 UAC 提权）

## 注册步骤

以下示例假设：

- 二进制位于 `C:\Users\<name>\go\bin\deepseek-search-fetch-mcp.exe`
- 数据目录放到 `D:\data\deepseek`
- API Key 为 `sk-...`

```powershell
$svc = "DeepSeekSearchFetchMCP"
$bin = "C:\Users\<name>\go\bin\deepseek-search-fetch-mcp.exe"
$dir = "D:\data\deepseek"

nssm install $svc $bin
nssm set $svc AppParameters server
nssm set $svc AppDirectory (Split-Path $bin)
nssm set $svc AppEnvironmentExtra DEEPSEEK_API_KEY=sk-... DEEPSEEK_SEARCH_FETCH_HOME=$dir
nssm set $svc Start SERVICE_AUTO_START
nssm set $svc AppStdout "$dir\logs\nssm-stdout.log"
nssm set $svc AppStderr "$dir\logs\nssm-stderr.log"
nssm start $svc
```

说明：

- `AppParameters server`：服务启动时执行 `server` 子命令。
- `AppEnvironmentExtra DEEPSEEK_SEARCH_FETCH_HOME=...`：把配置、缓存、日志统一落到指定目录，替代默认的家目录推断。也可以改用 `--data-dir`，但那样需要写进 `AppParameters`（例如 `server --data-dir D:\data\deepseek`）；推荐用环境变量，意图更清晰。
- `AppStdout` / `AppStderr`：让 NSSM 捕获进程标准输出/错误到文件，可选；程序自身日志已写入 `<data_dir>/logs/server.log`。
- 路径或 Key 包含空格时，用引号包裹对应的 `KEY=VALUE` 整体。

## 管理访问密钥

`apikey` 命令和 Windows 服务必须使用同一个数据目录，否则服务看不到 CLI 写入的密钥。使用上面的示例时：

```powershell
$env:DEEPSEEK_SEARCH_FETCH_HOME = "D:\data\deepseek"
deepseek-search-fetch-mcp apikey add laptop
deepseek-search-fetch-mcp apikey list
deepseek-search-fetch-mcp apikey remove laptop
```

密钥保存在 `D:\data\deepseek\auth.db`。没有密钥时 `/mcp` 不鉴权；添加至少一个密钥后立即强制校验。增删密钥不需要重启服务，也不需要 DeepSeek API Key。

## 默认语言与 LANG

`default_lang: "auto"` 只读取 `LC_ALL`、`LC_MESSAGES`、`LANG` 环境变量，不读取 Windows 系统区域设置。NSSM 服务默认没有这些变量，`auto` 会解析为空并关闭自动语言补全。

需要语言自动补全时，有两种方式：

- 在 `config.json` 中显式写 `"default_lang": "zh"`（推荐，行为最可预测）；
- 或通过 NSSM 注入 `LANG`，例如 `AppEnvironmentExtra DEEPSEEK_API_KEY=sk-... DEEPSEEK_SEARCH_FETCH_HOME=D:\data\deepseek LANG=zh_CN.UTF-8`。此后 `"default_lang": "auto"` 会解析为 `zh`。

## 验证

```powershell
sc query $svc        # 状态应为 RUNNING
Invoke-WebRequest http://127.0.0.1:18081/health   # 应返回 200
```

启动后应能看到 `D:\data\deepseek\config.json`、`D:\data\deepseek\cache.db`、`D:\data\deepseek\auth.db`、`D:\data\deepseek\logs\server.log` 出现在同一目录。也可以做一次完整 MCP 会话验证：`tools/list` 返回 `web_ext_search` / `web_ext_fetch`，`tools/call web_ext_search` 能真实返回上游结果（证明 API Key 确实来自数据目录下的配置文件）。

## 升级

`go install ...@latest` 只是替换 `bin` 下的可执行文件，正在运行的服务仍使用旧进程，必须重启才生效（服务指向的是 bin 路径，不是快照）：

```powershell
nssm restart $svc
```

## 日志位置

| 日志 | 路径 |
|---|---|
| 应用日志 | `<data_dir>/logs/server.log`（单文件 10 MiB，保留 5 个文件） |
| NSSM 捕获 stdout | `<data_dir>/logs/nssm-stdout.log`（需配置 `AppStdout`） |
| NSSM 捕获 stderr | `<data_dir>/logs/nssm-stderr.log`（需配置 `AppStderr`） |

## 卸载

```powershell
nssm stop $svc
nssm remove $svc confirm
```
