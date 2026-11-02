# Capybara 架构文档

实验室单机多卡 GPU 作业排队系统（Go 实现）｜版本 v0.1 草案｜2026-11-02

## 1. 背景与目标

### 1.1 背景

实验室有一台 GPU 服务器，每位同事在各自的 Docker 容器（`--gpus` 创建）中开发和测试。目前没有任务队列：有实验在跑时其他人无法运行自己的实验，需要人工确认 GPU 空闲后手动启动。

### 1.2 目标

- 用户在容器内提交命令，系统自动排队，GPU 空闲时由宿主机在**指定容器内**拉起任务。
- 以**单张 GPU 为粒度**分配，支持一个作业申请 N 张卡。
- 支持取消、超时、日志查看、守护进程重启后的状态恢复。
- 提供基本公平性（按用户近期用量排序）和配额限制。
- 部署简单：两个静态二进制 + 一个 systemd unit。

### 1.3 非目标（v1 不做）

- 多机集群调度（那是 Slurm / Kubernetes 的领域）。
- 硬性 GPU 隔离（见 7.3 的说明，`docker exec` 只能做软约束）。
- 作业抢占、检查点迁移。
- 复杂的计费与审批流程。

### 1.4 命名

- 守护进程：`capybarad`
- 命令行客户端：`capy`（`capybara` 的简写，便于输入）
- Socket：`/run/capybara/capybara.sock`

## 2. 总体架构

```
  ┌──────────── 容器 A ────────────┐   ┌──────────── 容器 B ────────────┐
  │  capy submit -g 2 -- python …  │   │  capy status / logs / cancel   │
  └───────────────┬────────────────┘   └───────────────┬────────────────┘
                  │  HTTP over unix socket（bind mount）│
                  ▼                                     ▼
  ┌─────────────────────────── 宿主机：capybarad ───────────────────────────┐
  │  API Server ── Identity（SO_PEERCRED → 容器）                           │
  │      │                                                                  │
  │      ▼                                                                  │
  │  Store（SQLite）◄────── Scheduler（定时 + 事件触发）                    │
  │                              │ 读取空闲 GPU：Allocator + NVML Monitor   │
  │                              ▼                                          │
  │                         Runner（每作业一个 goroutine）                  │
  │                              │ os/exec                                  │
  │                              ▼                                          │
  │      docker exec -e CUDA_VISIBLE_DEVICES=… -w … <容器> sh -c <wrapper>  │
  │                              │ stdout/stderr → /var/lib/capybara/logs   │
  └──────────────────────────────┴──────────────────────────────────────────┘
```

数据流概述：

1. 用户在容器里运行 `capy submit`，客户端采集 cwd、环境变量、命令，通过 unix socket 发给 `capybarad`。
2. `capybarad` 通过 socket 对端的 PID 识别出请求来自哪个容器，写入 SQLite，状态为 `pending`。
3. 调度器发现有足够的空闲 GPU 且用户未超额，则分配 GPU，状态变为 `running`，启动 Runner。
4. Runner 在宿主机执行 `docker exec`，监控退出码，写回状态，释放 GPU，触发下一轮调度。

## 3. 核心概念

### 3.1 作业（Job）

| 字段 | 说明 |
| --- | --- |
| `id` | 自增整数，用户可见 |
| `user` | 提交者标识（由容器映射而来，见 7.1） |
| `container` | 目标容器的 ID 与名称 |
| `cmd` | 命令参数数组（JSON），避免 shell 注入与转义问题 |
| `cwd` / `env` / `uid` | 提交时采集，执行时通过 `docker exec` 的 `-w`、`-e`、`-u` 还原 |
| `ngpus` | 申请的 GPU 数量，可为 0（纯 CPU 作业不排队，直接运行，可配置） |
| `priority` | 默认 0，管理员可调整 |
| `time_limit` | 最长运行时间，超时被终止 |
| `gpus` | 分配到的 GPU 编号，运行时才有值 |
| `state` | 见下方状态机 |
| `exit_code` / `reason` | 结束信息 |
| 时间戳 | `submit_at`、`start_at`、`end_at` |

### 3.2 状态机

```
  pending ──► running ──► succeeded
     │           │  ├───► failed        （退出码非 0）
     │           │  ├───► timeout       （超过 time_limit）
     │           │  └───► lost          （重启后无法确认结果）
     └───────────┴──────► cancelled
```

- `cancelled`、`succeeded`、`failed`、`timeout`、`lost` 为终态。
- 状态变更必须在同一个 SQLite 事务里同时更新 GPU 分配信息，保证不会出现"作业已结束但 GPU 仍被占用"的情况。

### 3.3 GPU 资源

启动时通过 NVML 枚举所有 GPU（索引、UUID、总显存）。调度使用 **UUID** 作为稳定标识，设置环境变量时再转换为索引，避免硬件重排后错乱。

每张卡有三种来源的"占用"判断：

- **内部分配**：Capybara 自己分配给作业的卡（权威来源）。
- **外部占用**：NVML 发现有不属于任何已知作业的进程占用显存，或显存使用超过阈值，视为"野任务"，该卡暂时不可调度并在 `capy nodes` 中提示。
- **禁用**：管理员手动下线（维护、坏卡）。

## 4. 代码结构

```
capybara/
├── cmd/
│   ├── capybarad/main.go       # 守护进程入口
│   └── capy/main.go            # CLI 入口
├── internal/
│   ├── api/                    # HTTP handler、请求/响应结构（客户端共用）
│   ├── identity/               # SO_PEERCRED、cgroup 解析、容器校验
│   ├── store/                  # SQLite 访问、迁移、事务封装
│   ├── scheduler/              # 调度循环、排序策略、配额
│   ├── gpu/                    # NVML 封装、分配表、外部占用检测
│   ├── runner/                 # docker exec 封装、取消、超时、日志
│   ├── notify/                 # webhook 通知（飞书/钉钉/邮件）
│   └── config/                 # 配置加载（YAML）
├── deploy/
│   ├── capybarad.service       # systemd unit
│   └── config.example.yaml
├── go.mod
└── Makefile                    # 静态编译：CGO_ENABLED=0
```

### 4.1 依赖选型

| 用途 | 选择 | 理由 |
| --- | --- | --- |
| HTTP | 标准库 `net/http`（Go 1.22+ 路由） | 无需第三方，监听 unix socket |
| CLI | `spf13/cobra` | 子命令成熟 |
| 数据库 | `modernc.org/sqlite` | 纯 Go，保持 `CGO_ENABLED=0` 静态编译 |
| GPU | `NVIDIA/go-nvml` | 官方绑定；其依赖动态加载 libnvidia-ml，守护进程跑在宿主机，无问题。MVP 阶段可先用解析 `nvidia-smi` 输出替代 |
| 日志 | 标准库 `log/slog` | 结构化日志 |
| 配置 | `gopkg.in/yaml.v3` | 简单直接 |
| 执行 | 调用 `docker` CLI（`os/exec`） | `docker exec` 的 TTY、信号、退出码行为与官方 CLI 一致，比 SDK 省事 |

> `capy` 客户端只依赖标准库与 cobra，保证单文件静态二进制，可直接挂进任意容器。

## 5. 通信协议

使用 **HTTP/JSON over unix socket**。优点：调试可以直接 `curl --unix-socket`；后续加网页看板、Prometheus 指标都顺理成章；协议变更向后兼容容易。

### 5.1 API 一览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/v1/jobs` | 提交作业 |
| GET | `/v1/jobs` | 列表，可按 `user`、`state` 过滤 |
| GET | `/v1/jobs/{id}` | 作业详情 |
| DELETE | `/v1/jobs/{id}` | 取消作业 |
| GET | `/v1/jobs/{id}/logs?follow=1` | 日志，支持流式跟随 |
| GET | `/v1/nodes` | 各 GPU 状态与占用者 |
| POST | `/v1/admin/gpus/{idx}/disable` | 管理员：禁用 GPU（需 root） |
| PATCH | `/v1/admin/jobs/{id}` | 管理员：调整优先级 |

### 5.2 提交请求示例

```json
{
  "cmd": ["python", "train.py", "--lr", "1e-4"],
  "cwd": "/workspace/exp1",
  "env": {"WANDB_API_KEY": "…", "PYTHONPATH": "/workspace"},
  "uid": 1000,
  "ngpus": 2,
  "time_limit": "48h"
}
```

注意：请求体里**不包含**用户名与容器名。这些信息由服务端通过连接身份得出，客户端无法伪造。

## 6. 数据模型（SQLite）

```sql
CREATE TABLE jobs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user        TEXT    NOT NULL,
  container_id   TEXT NOT NULL,
  container_name TEXT NOT NULL,
  cmd         TEXT    NOT NULL,         -- JSON 数组
  cwd         TEXT    NOT NULL DEFAULT '',
  env         TEXT    NOT NULL DEFAULT '{}',
  uid         INTEGER,
  ngpus       INTEGER NOT NULL DEFAULT 1,
  priority    INTEGER NOT NULL DEFAULT 0,
  time_limit  INTEGER,                  -- 秒
  state       TEXT    NOT NULL,
  gpus        TEXT,                     -- JSON，分配的 GPU UUID 列表
  runner_pid  INTEGER,                  -- docker exec 客户端的宿主机 PID
  exit_code   INTEGER,
  reason      TEXT,
  submit_at   INTEGER NOT NULL,
  start_at    INTEGER,
  end_at      INTEGER
);
CREATE INDEX idx_jobs_state ON jobs(state, priority DESC, submit_at);
CREATE INDEX idx_jobs_user  ON jobs(user, end_at);

CREATE TABLE gpus (
  uuid     TEXT PRIMARY KEY,
  idx      INTEGER NOT NULL,
  disabled INTEGER NOT NULL DEFAULT 0,
  note     TEXT
);

CREATE TABLE usage_log (        -- 公平份额统计用
  job_id   INTEGER PRIMARY KEY,
  user     TEXT NOT NULL,
  gpu_secs INTEGER NOT NULL,    -- 运行秒数 × GPU 数
  end_at   INTEGER NOT NULL
);
```

要点：

- 开启 WAL：`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`。
- SQLite 单写者，Store 层用一个写连接加互斥即可，对这个规模绰绰有余。
- 以后若要保留历史，可定期把终态作业归档到另一个文件。

## 7. 关键模块设计

### 7.1 身份识别（identity）

目标：确定一个请求来自**哪个容器**、**哪个用户**，不依赖客户端自报。

步骤：

1. 在 unix socket 连接上用 `SO_PEERCRED` 取得对端 PID。内核会把 PID 翻译到 `capybarad` 所在（宿主机）PID 命名空间，所以拿到的是宿主机 PID。
2. 读取 `/proc/<pid>/cgroup`，从路径中解析出 Docker 容器 ID（cgroup v1：`/docker/<id>`；cgroup v2 + systemd：`docker-<id>.scope`）。若无法解析，说明请求来自宿主机本身，按宿主机用户处理（用 `SO_PEERCRED` 的 UID）或直接拒绝（可配置）。
3. 执行 `docker inspect <id>` 获得容器名、状态以及 label。
4. 用户映射：优先读取容器 label `capybara.user=<name>`（建议每人创建容器时带上），其次回退到 `docker.name` 与配置文件里的映射表。

Go 中取得对端凭据的方式：

```go
// ConnContext 钩子里拿到 *net.UnixConn
raw, _ := uc.SyscallConn()
var cred *unix.Ucred
raw.Control(func(fd uintptr) {
    cred, _ = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
})
// cred.Pid, cred.Uid
```

结论：**用户只能往自己所在的容器提交作业**，彻底避免"在别人的容器里执行命令"。

### 7.2 调度器（scheduler）

触发时机：新作业提交、作业结束、作业取消、GPU 状态变化，外加每 5 秒的兜底定时器。

排序策略（v1）：

1. 先按 `priority` 降序；
2. 同优先级按用户**近 7 天衰减用量**升序（用得少的优先）；
3. 再按提交时间升序。

分配规则：

- 过滤掉被禁用、被外部占用的 GPU，得到可用集合。
- 检查用户配额：最大并发作业数、最大同时占用 GPU 数（配置文件设置）。
- 采用**严格队首优先 + 有限回填**：队首作业放不下时，为它预留，后面较小的作业只有在"不会延迟队首预期开始时间"（即其 `time_limit` 小于队首预计等待时间）时才能回填。v1 可先只实现严格队首优先，以避免大作业饿死，回填放到后续版本。
- 多卡作业尽量选 NVLink 拓扑相邻的卡（v2，通过 NVML 拓扑信息）。

核心骨架：

```go
func (s *Scheduler) tick(ctx context.Context) {
    s.mu.Lock(); defer s.mu.Unlock()

    free := s.gpus.Available()            // 去除已分配/禁用/外部占用
    queue := s.store.Pending(s.policy)    // 已按策略排好序

    for _, j := range queue {
        if !s.quota.Allow(j.User, j.NGPUs) { continue }
        if len(free) < j.NGPUs {
            break                          // 严格队首优先：放不下就等待
        }
        alloc := free[:j.NGPUs]
        free = free[j.NGPUs:]
        if err := s.store.MarkRunning(j.ID, alloc); err != nil { continue }
        go s.runner.Run(ctx, j, alloc)
    }
}
```

### 7.3 GPU 管理（gpu）与隔离边界

- **分配表**以数据库为准，内存中保留缓存。
- **NVML Monitor** 每 2–5 秒采样：显存占用、GPU 上的进程列表。将进程 PID 映射回容器（同 7.1 的 cgroup 方法），若发现某卡上有进程不属于任何 `running` 作业，则标记为 `external` 并记日志，必要时通过 webhook 提醒。
- 作业通过 `CUDA_VISIBLE_DEVICES=<uuid列表>` 限定可见卡（CUDA 支持 UUID 写法）。

> **重要说明**：容器是用 `--gpus all` 创建的，`docker exec` 无法在运行时改变容器可见的设备。因此隔离是**软约束**，用户若绕过 Capybara 直接在容器里跑程序，系统无法阻止，只能通过 NVML 检测并告警。如需硬隔离，需改为每个作业 `docker run --gpus device=N` 新起容器，代价是丢失容器内的交互状态，列入 v2 的可选模式。

### 7.4 执行器（runner）

对每个作业：

1. 创建日志文件 `/var/lib/capybara/logs/<job_id>.log`。
2. 拼接命令：

```go
args := []string{"exec",
    "-u", uid, "-w", cwd,
    "-e", "CUDA_VISIBLE_DEVICES=" + strings.Join(gpuUUIDs, ","),
    "-e", "CAPYBARA_JOB_ID=" + id,
}
for k, v := range env { args = append(args, "-e", k+"="+v) }
args = append(args, containerID, "sh", "-c", wrapperScript, "capy-wrapper")
args = append(args, cmd...)   // 作为位置参数，由 wrapper 用 "$@" 执行，避免注入
cmdExec := exec.CommandContext(ctx, "docker", args...)
cmdExec.Stdout, cmdExec.Stderr = logFile, logFile
```

3. `wrapperScript` 在容器内做两件事：记录进程组、用 `setsid` 开新会话后执行真实命令：

```sh
mkdir -p /tmp/capybara
echo $$ > /tmp/capybara/$CAPYBARA_JOB_ID.pid
exec setsid "$@"
```

由于 `exec` 保持 PID 不变，`$$` 同时也是新会话/进程组的组长，取消时对其负的 PID 发信号即可杀死整个进程树。镜像里没有 `setsid` 时退化为只记录 PID 并给出警告。

4. 取消流程：先 `docker exec <容器> sh -c 'kill -TERM -- -$(cat /tmp/capybara/<id>.pid)'`，等待宽限期（默认 30 秒），仍未退出则改发 `KILL`，最后终止宿主机上的 `docker exec` 客户端进程。 注意：仅仅 kill 宿主机上的 `docker exec` 客户端**不会**结束容器内的进程，必须按上面的方式处理。
5. 超时：`time.AfterFunc(timeLimit, cancel)`，终态记为 `timeout`。
6. 结束：解析退出码，在一个事务里写入终态、释放 GPU、写 `usage_log`，然后唤醒调度器并触发通知。

已知细节：`docker exec` 返回 125/126/127 时表示 Docker 自身或命令不存在的错误，应在 `reason` 中给出明确提示（如"容器未运行""命令未找到"）。

### 7.5 重启恢复

`capybarad` 重启（升级、崩溃）不应影响正在跑的作业：

- Runner 启动的 `docker exec` 子进程在启动时设置 `Setpgid` 且不绑定守护进程生命周期，由 systemd 配置 `KillMode=process`，避免重启时杀掉整个 cgroup 里的作业。
- 每个作业在宿主机创建一个状态目录 `/var/lib/capybara/jobs/<id>/`，其中包含 `exit_code` 文件。为了让 `docker exec` 在守护进程退出后依然可以记录退出码，实际执行命令外面再套一层小的 `capybara-shim`（同一个二进制的子命令，`capybarad shim <id> -- docker exec …`），由 shim 负责等待并写入 `exit_code`。
- 启动时对账：
  - 读取所有 `running` 作业；
  - 若 shim 进程仍存活（检查 `runner_pid`），重新接管，继续监控；
  - 若 shim 已退出且存在 `exit_code`，补写终态；
  - 两者都没有，则标记为 `lost` 并释放 GPU，同时通知用户。
- 同时对 `pending` 作业不做任何处理，直接继续排队。

## 8. 命令行（capy）

```
capy submit -g 2 [-t 24h] [-p 0] -- python train.py   # 提交
capy ls [-a] [--user alice] [--state running]         # 查看队列
capy info <id>                                        # 详情
capy logs [-f] <id>                                   # 查看/跟随日志
capy cancel <id>                                      # 取消
capy nodes                                            # GPU 占用与外部进程
capy admin disable-gpu <idx> | enable-gpu <idx>       # 管理员
```

`capy submit` 行为：

- 默认捕获当前 cwd，并按白名单或用户 `--env` 参数传递环境变量（默认传递除 `PATH`、`HOME` 等容器自身变量以外的全部变量，可配置）。
- 命令放在 `--` 之后，原样作为参数数组传输。
- 提交成功后输出作业 ID 和队列位置。
- 加 `--wait` 则阻塞直到作业结束，并以作业的退出码退出，方便写脚本。

容器挂载示例：

```bash
docker run -d --gpus all --label capybara.user=alice \
  -v /run/capybara/capybara.sock:/run/capybara/capybara.sock \
  -v /usr/local/bin/capy:/usr/local/bin/capy:ro \
  --name alice-dev <image>
```

## 9. 可观测性与通知

- **日志**：作业输出落盘，`capy logs -f` 通过 HTTP 分块传输实现跟随；日志保留期可配置（默认 30 天）。
- **守护进程日志**：`slog` 输出 JSON 到 journald。
- **指标**（v2）：`/metrics` 暴露 Prometheus 格式（队列长度、GPU 利用率、等待时间分布），配 Grafana 看板。
- **通知**：作业开始、结束、失败、超时、`lost` 时，向用户配置的 webhook（飞书/钉钉/企业微信）或邮件发送消息；用户通过 `capy notify set <url>` 配置，存放于数据库。

## 10. 部署

### 10.1 systemd

```ini
[Unit]
Description=Capybara GPU job queue
After=docker.service nvidia-persistenced.service
Requires=docker.service

[Service]
ExecStart=/usr/local/bin/capybarad --config /etc/capybara/config.yaml
RuntimeDirectory=capybara
StateDirectory=capybara
KillMode=process
Restart=on-failure
# 需要 root 或 docker 组权限执行 docker exec
User=root

[Install]
WantedBy=multi-user.target
```

### 10.2 配置文件

```yaml
socket: /run/capybara/capybara.sock
socket_mode: "0666"          # 容器内用户都能连接；权限控制由身份识别负责
data_dir: /var/lib/capybara
schedule_interval: 5s
default_time_limit: 72h
log_retention: 720h

quota:
  default: { max_running_jobs: 2, max_gpus: 4 }
  users:
    alice: { max_gpus: 8 }

users:                       # 容器名到用户的回退映射
  alice-dev: alice

nvml:
  external_use_threshold_mib: 500   # 超过此显存且不属于任何作业，视为野任务

notify:
  default_webhook: ""
```

## 11. 安全考量

- `capybarad` 以 root 运行并代为执行 `docker exec`，属于高权限组件，必须保证：命令参数以数组传递，**不拼接 shell 字符串**；容器 ID 来自内核提供的 cgroup 信息，而非用户输入。
- 提交者只能操作自己的作业（取消、查看日志），管理员接口要求对端 UID 为 0。
- `socket_mode 0666` 的前提是身份识别可靠；若某些容器使用了 `--pid=host` 或自定义 cgroup 导致无法识别，则一律拒绝。
- 环境变量可能包含密钥，作业记录里对 `env` 做脱敏展示，并限制日志与数据库目录权限为 `0700`。
- 日志目录按用户隔离读取权限（通过 API 校验，而不是文件权限）。

## 12. 里程碑

| 阶段 | 内容 | 预计 |
| --- | --- | --- |
| **M1 MVP** | 提交、FIFO 排队、按卡分配、`docker exec` 执行、日志、取消、SQLite 持久化 | 3–5 天 |
| **M2 稳定性** | 身份识别、重启恢复（shim）、超时、外部占用检测、systemd | 3–4 天 |
| **M3 策略** | 公平份额、配额、优先级、通知 | 2–3 天 |
| **M4 体验** | `--wait`、Prometheus 指标、网页看板（只读）、回填调度 | 按需 |
| **M5 增强** | 拓扑感知、可选硬隔离模式（`docker run`）、作业依赖（`--after`） | 按需 |

## 13. 风险与待定问题

| 问题 | 影响 | 当前倾向 |
| --- | --- | --- |
| 用户绕过队列直接运行 | GPU 争用，作业 OOM | NVML 检测加告警，并在实验室制度上约定 |
| 容器内没有 `setsid`/`sh` | 取消无法杀进程树 | 提供检测命令 `capy doctor`，必要时在镜像里补装 |
| cgroup v1/v2 差异 | 身份识别失败 | 两种路径都实现，并做集成测试 |
| 容器被删除/重启 | 作业丢失 | 作业开始时检查容器状态；容器消失则标记失败并通知 |
| 交互式需求（调试、Jupyter） | 无法排队 | v1 只支持非交互批处理；交互式另设"独占预约"功能 |
| SQLite 损坏/磁盘满 | 系统不可用 | 定期备份 `capybara.db`，启动时 `PRAGMA integrity_check` |

待与实验室确认：

1. 服务器 GPU 数量与型号，是否有 NVLink。
2. 每位同学的容器是否可以统一加上 `capybara.user` label 和 socket 挂载。
3. 默认配额与超时时间的具体数值。
4. 通知渠道选择（飞书、钉钉还是邮件）。


## 14. 当前实现记录（2026-11-02）

已建立 Go 模块与两个命令入口，提供 M1 基础链路及身份校验、超时：提交、严格 FIFO、GPU UUID 分配、Docker 执行、日志跟随、取消和 SQLite 持久化。具体使用方式见 `README.md`。

本阶段与最终设计的差异：

- 数据库采用 `jobs`（状态、JSON 快照和单独保存的环境变量）与 `allocations`（UUID 唯一分配）两张表；状态与分配在同一事务中更新。尚未建立公平份额与 GPU 管理表。
- CLI 环境变量采用显式 `--env` 传递，避免默认上传密钥；输出暂为 JSON。
- GPU 使用 `nvidia-smi` 的显存阈值检测，尚未按进程确认外部占用。
- 未实现 shim。正常停止取消活跃作业，systemd 暂用 `KillMode=control-group`；异常退出或取消无法确认时保留运行记录和 GPU 分配，待人工检查。M2 实现 shim 后再切换为设计中的无中断重启模式。
- 暂未实现配额、优先级、公平份额、管理员接口、通知、日志清理、队列位置与 `--wait`。
- 当前拒绝宿主机提交，并要求请求 UID 与内核连接 UID 一致，尚不支持 user namespace UID 映射。
