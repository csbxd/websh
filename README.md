# WebSH

Go 驱动的浏览器 Shell，优先让 AI 浏览器代理可靠地操作。

WebSH 把命令、运行状态、退出码、工作目录、stdout 和 stderr 放在普通 HTML DOM 中。输入使用原生表单控件，输出使用可读取的文本元素，自动化不需要识别终端截图或模拟终端按键。后端仅使用 Go 标准库，前端无需构建，页面嵌入单个可执行文件。

## 快速开始

需要 Linux 或 macOS、Go 1.23+，以及 `/bin/sh`。

```sh
git clone https://github.com/csbxd/websh.git
cd websh
go run ./cmd/websh
```

打开启动日志中的地址，默认是 `http://127.0.0.1:8080`，在登录框填入日志显示的访问令牌。启动时没有设置令牌则自动生成；也可固定配置：

```sh
WEBSH_TOKEN='replace-with-a-long-random-secret' go run ./cmd/websh
```

构建独立程序：

```sh
go build -o websh ./cmd/websh
./websh -dir /path/to/workspace
```

| 参数 | 默认值 | 用途 |
| --- | --- | --- |
| `-addr` | `127.0.0.1:8080` | HTTP 监听地址 |
| `-shell` | `/bin/sh` | 每个会话使用的 Shell |
| `-dir` | 启动时的当前目录 | 新会话的默认工作目录 |
| `-output-limit` | `1048576` | 每条命令的每个输出流最多保留的字节数 |
| `-max-sessions` | `16` | 同时存活的会话上限 |
| `-hostname` | 空 | 额外允许的 HTTP Host 名称 |

## 嵌入现有 HTTP 服务

根包 `github.com/csbxd/websh` 提供 `Options` 和 `New`。`New` 返回 `http.Handler`、关闭函数和错误，不监听端口；宿主负责把 Handler 挂到自己的 HTTP 服务，并在退出时调用关闭函数。`Shell`、`Dir`、`OutputLimit` 和 `MaxSessions` 由宿主显式提供，输出上限范围为 `256` 到 `16777216`，会话上限范围为 `1` 到 `128`。

可以通过 `Authenticate` 接入宿主已有的认证，例如 OIDC：

```go
handler, closeWebSH, err := websh.New(websh.Options{
	Shell: "/bin/sh", Dir: "/path/to/workspace",
	OutputLimit: 1 << 20, MaxSessions: 16,
	Hostname: "shell.example.com",
	Authenticate: func(r *http.Request) bool {
		return appAuthenticatedSession(r) // 宿主验证 OIDC 会话后返回 true。
	},
	LoginURL: "/_auth/login", LogoutURL: "/_auth/logout",
})
if err != nil {
	return err
}
defer closeWebSH()
mux.Handle("/", handler)
```

示例中的 `websh` 和 `http` 分别来自 `github.com/csbxd/websh` 和 `net/http`；`appAuthenticatedSession` 由宿主实现，登录和退出路由也由宿主处理。设置 `Authenticate` 后，WebSH 的每个请求都只依赖该回调认证，原生令牌、Bearer 和认证 Cookie 不再提供登录入口。`LoginURL` 和 `LogoutURL` 用于浏览器登录、认证失效和退出时导航到宿主路由。HTTP Host 和请求来源检查仍然生效。

每次 `New` 创建一个会话管理器，同一个 Handler 的所有已认证用户共享会话和命令记录；认证回调不会隔离不同用户的数据。需要用户隔离时，宿主应分别创建和路由各用户的 Handler。关闭函数会停止该 Handler 的全部 Shell 会话，可重复调用。不设置 `Authenticate` 时使用原生令牌认证，宿主必须通过 `Token` 提供有效令牌；独立 CLI 会从 `WEBSH_TOKEN` 读取令牌，缺省时自动生成。

## 浏览器操作

登录后创建会话，输入命令并点击 Run。每个会话是独立、持久的 Shell：`cd`、`export` 和 Shell 函数会延续到下一条命令。支持多行脚本和 heredoc。同一会话一次执行一条命令，不同会话可以独立执行。

每条命令都有独立记录，显示 stdout、stderr、退出码、工作目录和运行状态。输出在命令运行期间持续更新。命令框支持 Ctrl/Cmd+Enter，执行后保留文本，便于复查。界面仅记录命令结束前的输出；结束后关闭输出管道，后台进程继续写入可能收到 SIGPIPE。后台任务应明确将输出重定向到文件。

需要 stdin 的命令，例如 `read` 和 `cat`，可以通过输入框发送文本；选中添加换行可模拟提交一行，Send EOF 关闭当前命令的 stdin。每次请求最多发送 4096 字节。Interrupt 向会话进程组发送 SIGINT，若未能结束命令，会在短暂等待后终止整个会话。超时或超出输出上限也会停止命令。

这是适用于命令和文本交互的 Shell。它不提供 PTY，也不适合 `vim`、交互式 `top` 等全屏终端程序。进程环境设置 `TERM=dumb`、`NO_COLOR=1` 和 `CLICOLOR=0`；输出作为纯文本显示，不执行输出中的 HTML。界面清理 ANSI 转义序列，把回车转换为换行，其余控制字符显示为 `\xNN`，保留制表符和换行；发生转换时，可展开原始输出 JSON 读取 API 返回的原始文本。`exit`、`exec` 替换 Shell、`set -e` 后的命令失败或非交互式 Shell 的致命语法错误可能关闭会话，此时创建新会话继续工作。

## AI 浏览器代理约定

控件有稳定 ID、可访问名称和原生表单语义。建议采用以下流程：

1. 独立模式在 `#auth-token` 填入令牌并点击 `#login-button`；外部认证模式通过宿主登录页面或 `#external-login` 完成登录。
2. 使用 `#new-session` 打开创建表单，在 `#session-name`、`#session-cwd` 填写信息，点击 `#create-session`。
3. 在 `#command-input` 填入完整命令，可通过 `#timeout-seconds` 设置超时，点击 `#run-command` 一次。
4. 等待出现对应的命令记录，读取其 `data-command-id`，按该 ID 持续观察输出和状态。
5. 记录的 `data-status` 进入 `completed`、`interrupted`、`timed_out` 或 `failed` 后，读取退出码，再决定下一步。

`completed` 表示命令已结束，退出码仍可能非零。HTTP 请求返回成功只表示命令已提交；不能把提交成功当成执行成功。

| DOM 元素 / 属性 | 含义 |
| --- | --- |
| `#websh[data-authenticated]` | 是否登录 |
| `#websh[data-connected]` | 最近一次请求是否成功 |
| `#websh[data-active-session-id]` | 当前选中的会话 ID |
| `#websh[data-session-state]` | `idle`、`running` 或 `closed` |
| `#websh[data-active-command-id]` | 当前运行的命令 ID，空字符串表示没有 |
| `#websh[data-command-count]` | 当前会话已显示的命令数量 |
| `#websh[data-submission-uncertain]` | 命令提交结果是否尚未确认 |
| `#command-ID` | 一条命令的 `article`；ID 为服务端返回的命令 ID |
| `#command-ID[data-status]` | 命令状态 |
| `#command-ID[data-exit-code]` | 完成后的数值退出码；运行中为空 |
| `#command-ID[data-cwd]` | 命令完成后的工作目录 |
| `#command-ID[data-truncated]` | 输出是否被截断 |
| `#command-ID[data-output-normalized]` | 显示的输出是否经过 ANSI / 控制字符转换 |
| `#stdout-ID` / `#stderr-ID` | 对应命令的可读输出文本；同时有 `data-stream` 属性 |
| `#raw-output-ID[data-field="raw-output"]` | 转换发生时显示 `{ "stdout": "…", "stderr": "…" }` JSON，解析后得到 API 的原始文本 |
| `#session-state` | 当前会话的可见状态信息 |

其他控件：`#session-select` 切换会话，`#refresh-session` 刷新，`#close-session` 关闭；`#stdin-input` 输入 stdin，`#stdin-newline` 控制追加换行，`#send-input` 发送，`#send-eof` 发送 EOF，`#interrupt-command` 中断。

`#session-state` JSON 提供 `stdout_element`、`stderr_element` 和 `raw_output_element` 选择器，以及 `output_normalized` 标记，可直接定位输出。运行中每 300 ms 刷新，空闲时每 1500 ms 刷新。客户端记录命令提交过程中的不确定状态，避免网络错误后自动重复执行；代理应先读取会话记录，确认是否已经执行。如果确认未执行，可通过 `#resolve-submission` 明确解锁后重试。不要因一次响应丢失就再次点击 Run。

## HTTP API

独立 CLI 的浏览器登录使用 HttpOnly、SameSite=Strict 的会话 Cookie；直接访问 API 也可使用 `Authorization: Bearer TOKEN`。令牌不放在 URL 中。接口返回 JSON。嵌入模式使用 `Authenticate` 时，认证由宿主负责，原生令牌登录接口和这些认证凭据不再生效。

| 方法与路径 | 请求 / 结果 |
| --- | --- |
| `GET /api/info` | 获取服务和认证状态；独立模式无需登录，外部认证模式仍校验宿主身份 |
| `POST /api/login` | 独立模式通过 `{ "token": "…" }` 设置认证 Cookie；外部认证模式不提供此入口 |
| `GET /api/sessions` | `{ "sessions": [...] }` |
| `POST /api/sessions` | `{ "name": "…", "cwd": "…" }`，返回会话；字段可省略 |
| `GET /api/sessions/{id}` | 会话快照，含命令记录和当前输出 |
| `DELETE /api/sessions/{id}` | 关闭会话，保留最后状态和命令记录 |
| `POST /api/sessions/{id}/commands` | `{ "command": "…", "timeout_seconds": 30 }`，返回已提交的命令 |
| `POST /api/sessions/{id}/input` | `{ "data": "hello\n", "eof": false }` |
| `POST /api/sessions/{id}/interrupt` | `{}`，中断运行中的命令 |

`timeout_seconds` 范围是 `0` 到 `86400`，支持小数；可省略或设为 `0`，表示不设置超时。会话运行命令时，再提交命令返回 `409 Conflict`。命令记录包含 `id`、`command`、`status`、`stdout`、`stderr`、`exit_code`、`cwd`、`started_at`、`finished_at` 和 `truncated`；运行中的 `exit_code` 为 `null`。

使用 Cookie 的示例：

```sh
curl -sS -c cookies.txt \
  -H 'Content-Type: application/json' \
  -d '{"token":"YOUR_TOKEN"}' \
  http://127.0.0.1:8080/api/login

curl -sS -b cookies.txt \
  -H 'Content-Type: application/json' \
  -d '{"name":"agent"}' \
  http://127.0.0.1:8080/api/sessions

# 将返回值中的会话 id 替换到 SESSION_ID。
curl -sS -b cookies.txt \
  -H 'Content-Type: application/json' \
  -d '{"command":"pwd; printf hello","timeout_seconds":30}' \
  http://127.0.0.1:8080/api/sessions/SESSION_ID/commands

curl -sS -b cookies.txt \
  http://127.0.0.1:8080/api/sessions/SESSION_ID
```

服务在内存中保留每个会话最近至多 100 条命令，历史输出预算为 4 MiB；超过预算时移除旧记录，最新一条命令始终保留。stdout 和 stderr 分别限制大小，超限时标记 `truncated=true` 并停止命令。关闭会话会保留记录；创建新会话时，累计会话数达到上限会清理关闭的会话。退出服务后会话和记录不持久化。

## 访问边界

WebSH 的命令拥有运行服务的操作系统用户权限，可读取、修改该用户的文件和执行程序。它没有沙箱，已认证用户或代理应当可信。独立 CLI 的所有监听地址都需要令牌认证；默认仅监听本机，写入请求检查来源。嵌入模式可由宿主通过 `Authenticate` 提供认证。HTTP Host 默认仅接受 `localhost` 或 IP 地址，使用自定义域名需要通过 CLI 的 `-hostname` 或库的 `Hostname` 显式配置。

远程使用可以保持服务监听 `127.0.0.1`，通过 SSH 隧道访问：

```sh
ssh -L 8080:127.0.0.1:8080 user@server
```

然后在本机浏览器打开 `http://127.0.0.1:8080`。HTTP 连接本身不加密，应通过 SSH 隧道保护远程连接中的令牌和会话 Cookie。避免使用 root 启动。

## 开发与验证

```sh
go test -race ./...
go vet ./...
go build -o websh ./cmd/websh
node --check web/app.js
```

集成测试通过真实 `/bin/sh` 和 HTTP 验证持久状态、多行 heredoc、stdout/stderr 分离、非零退出码、运行期间输出、stdin/EOF、忙碌冲突、中断、超时、Shell 退出、输出上限，以及认证和跨来源请求保护。GitHub Actions 在 Go 1.23 和当前稳定版本上运行这些检查。
