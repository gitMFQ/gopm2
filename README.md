# gopm2

[![CI](https://github.com/gitMFQ/gopm2/actions/workflows/ci.yml/badge.svg)](https://github.com/gitMFQ/gopm2/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/gitMFQ/gopm2?display_name=tag)](https://github.com/gitMFQ/gopm2/releases)
[![Go](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macOS%20%7C%20windows-lightgrey)](#安装)

**gopm2** 是一个用 Go 编写的轻量级进程管理器（类 PM2），面向单机常驻服务：声明式配置、崩溃自动重启、健康检查、日志轮转、指标采集与 Web 面板，全部打包进两个零依赖的静态二进制。

```
┌──────────┐   Unix socket (0600)   ┌───────────────┐   fork/exec   ┌─────────────┐
│  gopm2   │ ─────────────────────► │    gopm2d     │ ────────────► │  受管进程    │
│  CLI     │ ◄───────────────────── │   常驻 daemon  │ ◄──────────── │  app / worker│
└──────────┘      JSON / SSE        └───────┬───────┘   SIGTERM/KILL └─────────────┘
                                           │
                          ┌────────────────┼─────────────────┐
                          ▼                ▼                 ▼
                    ┌──────────┐    ┌────────────┐    ┌──────────────┐
                    │  bbolt   │    │ 日志 + 轮转 │    │ Web 面板      │
                    │ 状态/快照 │    │ 10MB × 5   │    │ :9615 + 认证  │
                    └──────────┘    └────────────┘    └──────────────┘
```

## 特性

- **双二进制、零依赖**：`gopm2`（CLI）+ `gopm2d`（常驻守护进程），单文件静态编译，无运行时依赖。
- **崩溃自动重启**：支持 `always` / `on-failure` / `never` 三种策略，可限制最大重启次数；重启采用指数退避（1s → 2s → 4s … 上限 30s），避免崩溃循环打爆 CPU。
- **健康检查**：`http` / `tcp` / `exec` 三种探针，连续失败达阈值触发重启。
- **声明式配置**：TOML 描述全部应用，`gopm2 apply` 幂等收敛（仅重启真正发生变化的进程）。
- **优雅停止**：先发 `SIGTERM` 给整个进程组，等待 `stop_timeout`（默认 10s）后再 `SIGKILL`，避免残留孤儿进程。
- **日志轮转**：stdout / stderr 分离落盘，按大小轮转（默认单文件 10MB，保留 5 份），支持 `tail -f` 式实时跟随。
- **指标采集**：后台每 5s 采样 CPU% 与 RSS（读 `/proc`），列表接口直接返回缓存值，不产生额外磁盘写。
- **实时事件流**：基于 SSE 推送状态变更，面板与外部消费者都能即时感知。
- **Web 面板**：内嵌静态资源，bcrypt 口令 + 24h session（Cookie 或 Bearer Token）双重校验，默认只监听 `127.0.0.1`。
- **快照与迁移**：`save` / `resurrect` 保存与恢复期望运行的进程集合，daemon 重启后自动恢复；`export` / `import` 支持整体备份迁移。
- **daemon 自愈**：`watchdog` 子进程监视 daemon，异常退出时自动拉起。
- **安全默认值**：控制通道 Unix socket 权限 `0600`，数据目录 `0700`，数据库 `0600`；`export`（含明文环境变量）仅在本地 socket 通道暴露，Web 侧不可访问；打印配置时对 `KEY`/`SECRET`/`TOKEN`/`PASSWORD` 类环境变量自动打码。

## 架构

| 组件 | 路径 | 职责 |
| --- | --- | --- |
| CLI | `cmd/gopm2` | 命令解析、daemon 惰性拉起、表格化输出、日志跟随 |
| Daemon | `cmd/gopm2d` | 单实例锁、监听 socket/TCP、resurrect、watchdog |
| Supervisor | `internal/supervisor` | 进程启停、重启退避、进程组信号、日志写入、指标采样 |
| Service | `internal/service` | 业务编排：apply 幂等、save/resurrect、export/import |
| API | `internal/api` | chi 路由、REST + SSE、认证中间件、Unix/TCP 双通道 |
| Store | `internal/store` | bbolt 封装（`apps` / `snapshot` / `auth` / `meta` 四个 bucket） |
| Health | `internal/health` | http / tcp / exec 探针 |
| Logutil | `internal/logutil` | 大小轮转写入器、日志尾部读取、实时跟随 |
| Metrics | `internal/metrics` | `/proc` 解析：CPU 时间、RSS、进程启动时间 |
| Web | `internal/web` | `go:embed` 内嵌面板静态资源 |

CLI 与 daemon 之间通过 **Unix socket** 通信（`0600`，仅本机同用户可访问，因此免认证）；Web 面板走 **TCP**，必须登录。CLI 在检测到 daemon 未运行时会自动 `EnsureDaemon` 拉起，无需手动启动。

## 安装

### 方式一：下载预编译二进制（推荐）

从 [Releases](https://github.com/gitMFQ/gopm2/releases) 下载对应平台归档，解压后将 `gopm2` 与 `gopm2d` **放在同一目录**（CLI 默认在同目录查找 daemon）：

```bash
tar -xzf gopm2_0.1.0_linux_amd64.tar.gz
sudo install -m 0755 gopm2 gopm2d /usr/local/bin/

gopm2 version
```

支持的目标平台：`linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`。

### 方式二：go install

```bash
go install github.com/gitMFQ/gopm2/cmd/gopm2@latest
go install github.com/gitMFQ/gopm2/cmd/gopm2d@latest
```

> 两个二进制都会安装到 `$(go env GOPATH)/bin`，满足"同目录"约定。

### 方式三：源码构建

```bash
git clone https://github.com/gitMFQ/gopm2.git
cd gopm2
make build          # 产物在 bin/
sudo make install   # 安装到 /usr/local/bin
```

需要 Go 1.26 及以上。

## 快速开始

```bash
# 1. 启动并守护一个进程（daemon 会自动拉起）
gopm2 start web --cwd ./services/api --env NODE_ENV=production --restart always -- node server.js

# 2. 查看进程列表
gopm2 list
# NAME  STATE   PID    CPU%  RSS    RESTARTS  UPTIME
# web   online  20431  0.3   48.2MB 0         12s

# 3. 实时跟随日志
gopm2 logs web -f          # stdout
gopm2 logs web --err       # stderr

# 4. 重启 / 停止 / 删除
gopm2 restart web
gopm2 stop web
gopm2 delete web
```

用仓库自带的 demo 观察自动重启行为：

```bash
make build
gopm2 start demo --restart on-failure -- ./bin/demo --crash-after 5s
gopm2 status demo          # 观察 restarts 计数与指数退避重启
gopm2 logs demo --err
```

从 TOML 批量声明并幂等收敛：

```bash
gopm2 apply gopm2.toml            # 创建/更新配置中的应用
gopm2 apply gopm2.toml --prune    # 同时删除配置里已不存在的应用
```

打开 Web 面板：

```bash
gopm2 dashboard            # 打印地址并尽力唤起浏览器
# → http://127.0.0.1:9615
# 初始密码在 daemon 首次启动日志中打印一次，也写在 <数据目录>/initial-password
```

## 命令参考

| 命令 | 说明 |
| --- | --- |
| `gopm2 start <name> [flags] -- <cmd...>` | 启动并守护进程（自动拉起 daemon） |
| `gopm2 stop <name>` | 停止（SIGTERM → 超时后 SIGKILL） |
| `gopm2 restart <name>` | 手动重启 |
| `gopm2 delete <name>` | 停止并从状态库删除 |
| `gopm2 list` / `gopm2 ls` | 进程列表（状态 / PID / CPU% / 内存 / 重启次数 / 运行时长） |
| `gopm2 status <name>` | 单个应用详情（JSON） |
| `gopm2 logs <name> [-f] [--err] [--stream out\|err]` | 查看或跟随日志 |
| `gopm2 save` | 把当前进程集合保存为快照 |
| `gopm2 resurrect` | 从快照恢复并启动 |
| `gopm2 apply <file.toml> [--prune]` | 幂等应用 TOML 配置 |
| `gopm2 export` | 导出全部应用规格（JSON，仅本地 socket） |
| `gopm2 import <file.json>` | 从 JSON 导入应用规格 |
| `gopm2 dashboard` | 打印 Web 面板地址并尝试打开浏览器 |
| `gopm2 daemon start\|ensure\|stop\|status` | 手动管理 daemon |
| `gopm2 version` | 打印版本 |
| `gopm2 install-cron` / `uninstall-cron` | **尚未实现**（当前仅占位提示） |

`start` 支持的参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--cwd <dir>` | 当前目录 | 工作目录 |
| `--env K=V` | — | 环境变量，可重复 |
| `--restart <policy>` | `on-failure` | `always` / `on-failure` / `never` |
| `--max-restarts <n>` | `0` | 最大重启次数，`0` 表示无限 |
| `--health-cmd <cmd>` | — | 健康检查命令（exec 探针） |
| `--health-interval <dur>` | `10s` | 健康检查间隔 |
| `--no-start` | `false` | 只注册不启动 |

## TOML 配置

`gopm2.toml` 顶层为 `version` + `[defaults]` + 若干 `[[apps]]`；`apply` 时会先套用 `defaults`，应用级字段可覆盖。相对 `cwd` 相对**配置文件所在目录**解析。

```toml
version = 1

[defaults]
restart_policy = "on-failure"
max_restarts = 10

[[apps]]
name = "api"
command = "node server.js"
cwd = "./services/api"
restart_policy = "always"
health_cmd = "curl -fsS http://127.0.0.1:3000/health"
health_interval = "15s"

[apps.env]
NODE_ENV = "production"
PORT = "3000"

[[apps]]
name = "worker"
command = "python worker.py"
restart_policy = "on-failure"
max_restarts = 5
```

可用的应用级字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `name` | string | 应用名（必填，唯一） |
| `command` | string | 命令（必填） |
| `args` | []string | 参数；非空时与 `command` 一起直接 `exec`，不经 shell |
| `cwd` | string | 工作目录，相对路径基于配置文件目录 |
| `env` | table | 环境变量 |
| `restart_policy` | string | `always` / `on-failure` / `never`，默认继承 `defaults` |
| `max_restarts` | int | 最大重启次数，`0` = 无限 |
| `health_cmd` | string | exec 健康检查命令 |
| `health_http` | string | http 健康检查 URL |
| `health_tcp` | string | tcp 健康检查地址 `host:port` |
| `health_interval` | string | 检查间隔，如 `"10s"` |

> 三种健康检查为**三选一**，同时配置时按 `health_cmd` → `health_http` → `health_tcp` 的优先级取第一个。

### 默认值与运行参数

| 项 | 默认值 |
| --- | --- |
| 重启退避 | 1s、2s、4s、8s、16s，上限 30s |
| 停止宽限 `stop_timeout` | 10s |
| 健康检查间隔 / 超时 / 失败阈值 | 10s / 5s / 3 次 |
| 日志轮转 | 单文件 10MB，保留 5 份 |
| 指标采样间隔 | 5s |
| Web 面板监听 | `127.0.0.1:9615` |
| 数据目录 | `~/.gopm2` |
| session 有效期 | 24h |

## Web 面板

面板静态资源通过 `go:embed` 编译进 `gopm2d`，无需额外部署。功能包括：

- 应用列表：状态、PID、CPU%、内存、重启次数、运行时长
- 操作：启动 / 重启 / 停止 / 删除
- 详情抽屉：规格与环境变量（密码类自动打码）
- 日志查看：stdout / stderr 切换、实时跟随、清屏
- 全局：`save` / `resurrect`、连接状态指示灯、退出登录
- 状态变化通过 `GET /api/events`（SSE）实时刷新

**认证**：首次启动时生成随机管理员密码，仅在日志中打印一次并写入 `<数据目录>/initial-password`（`0600`）。登录后签发 24h session；也可以带 `Authorization: Bearer <token>` 调用 API。密码以 bcrypt 哈希存储。

**安全提示**：面板默认只绑定 `127.0.0.1`。若改为对外监听（`--tcp 0.0.0.0:9615`），请自行置于反向代理与 TLS 之后，并确保口令强度。

## HTTP API

所有接口在 `/api` 下，`/healthz` 无需认证。TCP 通道需登录，Unix socket 通道（CLI 使用）免认证。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 健康检查 |
| POST | `/api/login` / `/api/logout` | 登录 / 登出（仅 TCP） |
| GET | `/api/apps` | 列出应用 |
| POST | `/api/apps` | 创建并启动应用 |
| GET | `/api/apps/{name}` | 应用详情 |
| DELETE | `/api/apps/{name}` | 删除应用 |
| POST | `/api/apps/{name}/start` | 启动 |
| POST | `/api/apps/{name}/stop` | 停止 |
| POST | `/api/apps/{name}/restart` | 重启 |
| GET | `/api/apps/{name}/logs` | 日志（支持 `stream`、`follow` 参数） |
| GET | `/api/apps/{name}/metrics` | 指标 |
| POST | `/api/save` / `/api/resurrect` | 保存快照 / 恢复 |
| POST | `/api/apply` | 应用 TOML 配置 |
| GET | `/api/export` | 导出规格（**仅 Unix socket**，含明文环境变量） |
| POST | `/api/import` | 导入规格 |
| GET | `/api/events` | SSE 状态事件流 |

## 环境变量

| 变量 | 作用域 | 说明 |
| --- | --- | --- |
| `GOPM2_DATA_DIR` | CLI + daemon | 数据目录，默认 `~/.gopm2` |
| `GOPM2_BIN` | CLI | 指定 `gopm2d` 路径（默认与 `gopm2` 同目录，其次查 `PATH`） |
| `GOPM2_TCP_ADDR` | daemon | 覆盖 Web 面板监听地址，默认 `127.0.0.1:9615` |

`gopm2d` 也可用 `--data-dir` 与 `--tcp`（`--tcp ""` 表示只开 CLI 通道）；端口被占用时面板降级不可用，但 CLI 通道不受影响。

## 数据目录

```
~/.gopm2/                      # 0700
├── gopm2.db                   # bbolt（0600）：apps / snapshot / auth / meta
├── gopm2.sock                 # CLI 控制通道（0600）
├── gopm2d.pid                 # 单实例 flock + pid
├── gopm2d-watchdog.pid        # watchdog pid
├── initial-password           # 首次启动生成的初始密码（0600）
└── logs/
    ├── <name>-out.log[.1..5]  # stdout（按 10MB 轮转）
    └── <name>-err.log[.1..5]  # stderr
```

## 开发

```bash
make build              # 构建 bin/gopm2、bin/gopm2d、bin/demo
make test               # 单元测试
make test-integration   # 集成测试（真实拉起 daemon，含 CLI + HTTP 全链路）
make vet                # go vet
make fmt                # gofmt -w .
make lint               # fmt + vet + test
make clean              # 清理 bin/
```

集成测试会为每个用例创建独立数据目录与随机 Web 端口，不会干扰本机已有实例。

## 发版流程

发布由 GitHub Actions 完成，配置见 [`.github/workflows/`](.github/workflows)：

- **CI**（`ci.yml`）：所有分支 push 与 PR 触发 —— `go mod verify`、`gofmt` 检查、`go vet`、`go test -race`、构建，以及独立 job 跑集成测试。
- **Release**（`release.yml`）：推送 `v*` tag（或手动触发并填 tag）后 —— 先跑完整校验（fmt/vet/单元测试/集成测试）作为发版门槛，再交叉编译 `linux|darwin|windows` × `amd64|arm64`，打包为 `tar.gz`（Windows 为 `zip`）并生成 `checksums.txt`，最后创建 GitHub Release（附自动生成变更说明）。

版本号通过 `-ldflags "-X main.version=<tag>"` 注入，因此 `gopm2 version` 与 `gopm2d -version` 都会报告真实发布版本。

手动发一个版本：

```bash
git tag -a v0.1.1 -m "v0.1.1"
git push origin v0.1.1
```

## 已知限制

- `install-cron` / `uninstall-cron` 尚未实现（计划用于 cron 保活）。
- Web 面板为单管理员口令模型，未提供多用户与权限分级。
- 面向**单机**进程管理，不涉及集群调度与跨机编排。

---

[![](https://img.shields.io/badge/powered_by-dsh-4D6BFE?style=flat-square&logo=deepseek&logoColor=white)](https://github.com/deepseek-ai/deepseek-harness)
