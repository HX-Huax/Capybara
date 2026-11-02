# Capybara

[English](README.md) | 简体中文

Go 实现的单机多卡 GPU 作业队列。当前提供 M1 基础实现：Unix socket HTTP API、Docker 容器身份识别、SQLite 持久化、严格 FIFO、按 GPU UUID 分配、执行、超时、取消与日志跟随。

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
capy submit -g 1 -t 24h --env PYTHONPATH -- python train.py
capy ls
capy info 1
capy logs -f 1
capy cancel 1
capy nodes
```

命令和参数原样传递，若要使用管道等 shell 语法，请显式提交 `sh -c '...'`。环境变量仅通过重复的 `--env KEY` 或 `--env KEY=VALUE` 显式传递。GPU 可见性与作业 ID 由服务端设置，不能通过 `--env` 覆盖。API 返回的作业详情不包含环境变量。

提交者必须属于可识别的 Docker cgroup，且容器有 `capybara.user` 标签或配置中的名称映射。宿主机请求直接拒绝；当前版本不支持 user namespace UID 映射。提交、详情和列表输出 JSON。

## 当前边界

- 排队采用严格 FIFO；队首 GPU 不够时，后续作业也会等待。尚未加入配额、公平份额、优先级和回填。
- GPU 探测使用 `nvidia-smi`，未分配且显存超过阈值的卡视为外部占用。尚未实现 NVML 进程归属判断；这是保守的显存阈值检测。探测失败时停止新的 GPU 分配。
- CUDA 可见性是软约束，容器内用户仍能绕过队列使用 GPU。
- 取消和超时先尝试容器内进程组 TERM，再 KILL。若无法确认终止，保留 `running` 状态与 GPU 分配并记录服务日志，需要管理员检查。
- 正常停止服务会取消活跃作业。异常退出后，旧 `running` 作业保留分配并等待人工确认；尚未实现 shim 接管与自动恢复。不要在旧作业仍运行时删除数据库或手动释放记录。
- 尚未实现管理员接口、通知、日志清理、`--wait` 和队列位置计算。详情接口的时间限制字段 `time_limit_ns` 为纳秒整数，提交接口仍使用 `24h` 这样的字符串。

单元测试覆盖持久化、事务分配冲突、状态竞争检查、cgroup 解析、shell 参数注入防护、取消失败时保留资源，以及 FIFO 调度。Docker/GPU 替身用于自动化测试；真实服务器上的容器执行、驱动兼容性和 GPU 运行尚需验证。

## 许可证

本项目采用 [MIT 许可证](LICENSE)。
