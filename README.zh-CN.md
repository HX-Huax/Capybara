# Capybara

[English](README.md) | 简体中文

Go 实现的单机多卡 GPU 作业队列。当前提供 M1 基础实现：Unix socket HTTP API、Docker 容器身份识别、SQLite 持久化、优先级调度、同级 FIFO、按 GPU UUID 分配、执行、超时、取消与日志跟随。

日常提交、查看标准输出和管理作业，请阅读[快速使用指南](docs/quickstart.zh-CN.md)。

## 构建与检查

需要 Linux、Go 1.22 或更新版本。宿主机运行环境还需要 Docker CLI 和 `nvidia-smi`；容器内需要 `sh`、`setsid`，以及支持进程组信号的 `kill`。

```sh
make build
make test
make vet
```

产物为 `bin/capy` 和 `bin/capybarad`，均使用 `CGO_ENABLED=0` 构建。若默认 Go 构建缓存不可写，可设置 `GOCACHE=/tmp/capybara-go-build`。

## 启动

把 `deploy/config.example.yaml` 复制到 `/etc/capybara/config.yaml`，按服务器环境调整。配置文件仅接受当前实现的字段，完整架构草案中的策略和通知配置尚未支持。

```sh
sudo ./bin/capybarad --config /etc/capybara/config.yaml
```

服务账号需要读取宿主机 `/proc` 并执行 Docker。`deploy/capybarad.service` 提供 systemd 模板。数据库和日志放在权限为 `0700` 的数据目录中。

创建开发容器时设置用户标签并挂载客户端与 socket：

```sh
docker run -d --gpus all --label capybara.user=alice \
  --mount type=bind,src=/run/capybara,dst=/run/capybara,readonly \
  -v "$PWD/bin/capy":/usr/local/bin/capy:ro \
  --name alice-dev YOUR_IMAGE
```

先启动守护进程，再创建容器。按上面的方式挂载整个 `/run/capybara` 目录。服务配置使用 `RuntimeDirectoryPreserve=yes`，停止、重启以及先停止再启动时都保留目录；守护进程只在原目录中重建 socket，容器无需重启即可重新连接。单独挂载 socket 文件会使容器在服务重启后仍引用旧 socket。

已有部署需要在没有活跃作业时，在宿主机更新服务配置：

```sh
sudo install -m 0644 deploy/capybarad.service /etc/systemd/system/capybarad.service
sudo systemctl daemon-reload
sudo systemctl restart capybarad
sudo systemctl show capybarad -p RuntimeDirectoryPreserve
```

最后一条命令应输出 `RuntimeDirectoryPreserve=yes`。如果容器已经挂载了被删除的旧目录，服务启动后需要再重启一次该容器；如果只挂载了 socket 文件，需要以目录挂载方式重新创建容器。`/run` 是临时存储，此配置不保证宿主机重启后保留原目录。

在容器内执行：

```sh
capy submit -g 1 -p 10 -t 24h --env PYTHONPATH -- python train.py
capy ls
capy info 1
capy logs -f 1
capy cancel 1
capy nodes
```

命令和参数原样传递，若要使用管道等 shell 语法，请显式提交 `sh -c '...'`。环境变量仅通过重复的 `--env KEY` 或 `--env KEY=VALUE` 显式传递。GPU 可见性与作业 ID 由服务端设置，不能通过 `--env` 覆盖。API 返回的作业详情不包含环境变量。

提交者必须属于可识别的 Docker cgroup，且容器有 `capybara.user` 标签或配置中的名称映射。宿主机请求直接拒绝；当前版本不支持 user namespace UID 映射。提交、详情和列表输出 JSON。

## 不重建容器接入

对于已有 `/home` 等持久化 bind mount 的容器，可以在宿主机执行：

```sh
sudo capybarad attach --container luoyi --user luoyi --mount /home --dry-run
sudo capybarad attach --container luoyi --user luoyi --mount /home
```

工具识别真实挂载源，安装客户端与容器内链接，追加真实 socket 和用户映射，再通过 SIGHUP 热加载。重复运行可更新客户端，不停止、不重启或重建 Docker 容器。首次使用前需升级守护进程与 systemd 模板；首次服务升级仍须等待作业结束后重启。

详见 [已有容器接入与升级](docs/existing-containers.zh-CN.md)，包括冲突文件处理、配置备份和热加载边界。

## 调度优先级

提交时使用 `capy submit -p 10 -- 命令`，范围为 **-100 至 100**，默认为 **0**，数值越大越优先。旧数据库记录缺少该字段时自动按 0 处理，无需迁移。优先级相同时按提交时间、作业 ID 排序；不抢占已经运行的作业，也不会绕过资源不足的队首。

管理员可以通过 Web 面板修改 **pending** 作业的优先级，也可以经 Unix socket 调用 `PATCH /v1/admin/jobs/{id}`，请求体为 `{"priority": 20}`；该接口需要对端 UID 为 0，且仍须通过容器身份校验。提交者可以为自己的新作业设置优先级；当前没有配额或优先级审批机制。

## Web 控制与监控面板

面板随 `capybarad` 二进制发布，无需 Node 服务或外部 CDN，提供：

- 全局作业统计、搜索、用户与状态筛选、分页。
- GPU 分配、显存、利用率及当前浏览器会话最近 60 次采样趋势。
- 作业详情、命令参数、执行时间、退出码和日志。
- 排队作业优先级调整、二次确认取消。
- 每 3 秒自动刷新，支持暂停和手动刷新。

作业仍从容器内通过 CLI 提交，以保留容器身份验证。Web 使用独立的**全局管理员令牌**，拥有所有用户作业的查看、日志、取消和优先级管理权限。

### 在服务器启用

默认不监听 Web 端口。在 `/etc/capybara/config.yaml` 中添加或修改：

```yaml
web:
  listen: "127.0.0.1:8080"
  token_env: CAPYBARA_WEB_TOKEN
```

在宿主机首次生成令牌文件：

```sh
sudo sh -c 'umask 077; printf "CAPYBARA_WEB_TOKEN=%s\n" "$(openssl rand -hex 32)" > /etc/capybara/web.env'
```

更新后的 systemd 服务会读取该可选环境文件。构建并更新服务，在没有活跃作业时重启：

```sh
make build
sudo install -m 0755 bin/capybarad /usr/local/bin/capybarad
sudo install -m 0644 deploy/capybarad.service /etc/systemd/system/capybarad.service
sudo systemctl daemon-reload
sudo systemctl restart capybarad
```

使用 `-p` 还需安装新版 `bin/capy`；若容器绑定挂载的是旧客户端文件，可能需要重启容器以加载替换后的文件。

在自己的电脑建立到服务器的 SSH 转发：

```sh
ssh -N -L 8080:127.0.0.1:8080 icca@服务器地址
```

浏览器打开 **http://127.0.0.1:8080**，输入 `/etc/capybara/web.env` 中等号后的令牌。监听仅允许 loopback IP，通过本机或 SSH 转发访问。令牌至少 32 字节，仅保留在页面内存中，刷新页面后需要重新登录；更新令牌文件后需重启服务生效。

日志轮询只读取末尾 64 KiB；利用率趋势来自真实设备采样，仅保留在当前页面会话中。GPU 探测失败时仍可查看队列和操作作业。

## 当前边界

- 按优先级降序、提交时间升序、ID 升序调度；队首 GPU 不够时，后续作业也会等待，不抢占运行中的任务。尚未加入配额、公平份额和回填。
- GPU 探测使用 `nvidia-smi`，未分配且显存超过阈值的卡视为外部占用。尚未实现 NVML 进程归属判断；这是保守的显存阈值检测。探测失败时停止新的 GPU 分配。
- CUDA 可见性是软约束，容器内用户仍能绕过队列使用 GPU。
- 取消和超时先尝试容器内进程组 TERM，再 KILL。若无法确认终止，保留 `running` 状态与 GPU 分配并记录服务日志，需要管理员检查。
- 正常停止服务会取消活跃作业。异常退出后，旧 `running` 作业保留分配并等待人工确认；尚未实现 shim 接管与自动恢复。不要在旧作业仍运行时删除数据库或手动释放记录。
- 尚未实现 GPU 管理员接口、通知、日志清理、`--wait` 和队列位置计算；已支持排队作业优先级管理。详情接口的时间限制字段 `time_limit_ns` 为纳秒整数，提交接口仍使用 `24h` 这样的字符串。

单元测试覆盖持久化、事务分配冲突、状态竞争检查、cgroup 解析、shell 参数注入防护、取消失败时保留资源，以及优先级排序、同级 FIFO、Web 认证、日志截尾和 GPU 指标解析。已通过竞态检测、静态检查、构建、Unix/Web 双入口冒烟测试和 Chromium 桌面/手机交互检查。Docker/GPU 替身用于自动化测试；真实服务器上的容器执行、驱动兼容性和 GPU 运行尚需验证。

## 许可证

本项目采用 [MIT 许可证](LICENSE)。
