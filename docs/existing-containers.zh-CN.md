# 已有 Docker 容器接入与升级

利用容器**已经存在的 bind mount**安装客户端与真实 Unix socket，不重建、不停止、不重启容器。实验环境、SSH 端口、数据挂载和容器可写层保持原样。

该方式适合当前实验室的映射：

```text
宿主机 /home/icca/users/luoyi/  →  容器 /home/
```

接入后路径为：

```text
宿主机                                       容器内
/home/icca/users/luoyi/.capybara/capy       →  /home/.capybara/capy
/home/icca/users/luoyi/.capybara/capybara.sock → /home/.capybara/capybara.sock

容器内链接：
/usr/local/bin/capy              → /home/.capybara/capy
/run/capybara/capybara.sock      → /home/.capybara/capybara.sock
```

每个目录中的 socket 都由同一个 `capybarad` 直接监听，不使用转发代理，保留原始 `SO_PEERCRED` 身份识别。

```
查询挂载映射 
docker inspect wangyang \
  --format '{{range .Mounts}}{{println .Source "->" .Destination}}{{end}}'
```

## 1. 首次升级守护进程

旧版尚不支持多 socket 和 SIGHUP。首次安装新版需要一次服务重启，请等待 Capybara 活跃作业结束后操作。**这不要求重启 Docker 容器**。现阶段仍未实现守护进程升级时无中断接管作业的 shim。

在服务器仓库目录执行：

```sh
git pull --ff-only
make test
make vet
make build

# 仅在没有活跃 Capybara 作业时停止服务。
sudo systemctl stop capybarad
sudo cp /usr/local/bin/capy /usr/local/bin/capy.previous
sudo cp /usr/local/bin/capybarad /usr/local/bin/capybarad.previous
sudo install -m 0755 bin/capy bin/capybarad /usr/local/bin/
sudo install -m 0644 deploy/capybarad.service /etc/systemd/system/capybarad.service
sudo systemctl daemon-reload
sudo systemctl start capybarad
sudo systemctl show capybarad --property=CanReload
```

最后一条应输出 `CanReload=yes`。保留原有配置、数据库、显存阈值和 Web 令牌，不必复制示例配置覆盖原文件。升级前也应保留现有配置与数据库备份。

## 2. 预检查现有容器

在宿主机执行，按实际容器名和挂载目标调整：

```sh
sudo capybarad attach --container luoyi --user luoyi --mount /home --dry-run
```

工具会读取 `docker inspect`，确认：

- 容器正在运行，并存在目标为 `/home` 的 bind mount。
- 容器有 `sh`、`setsid`、`readlink`、`ln`、`mkdir`、`mv`、`cat`。
- 客户端和 socket 链接位置没有冲突文件或其他链接。
- 已有 `capybara.user` 标签、配置映射与指定用户没有冲突。

只检查和输出路径计划，不安装客户端、不修改 YAML 或容器链接。该工具需要宿主机 root 权限；不会自动安装依赖。

## 3. 接入，无需重建容器

```sh
sudo capybarad attach --container luoyi --user luoyi --mount /home
```

工具自动完成：

1. 在挂载源中创建由 root 管理的 `.capybara` 目录。
2. 写入临时探针并从容器内读取，验证该目录确实可见。
3. 原子安装客户端，避免新进程执行到复制了一半的文件。
4. 保留 YAML 注释，追加 `additional_sockets` 和容器名到用户的映射。
5. 首次将原配置备份为 `config.yaml.before-attach`，后续不覆盖该备份。
6. 在容器内建立标准客户端和 socket 链接。
7. 调用 `systemctl reload capybarad`，等待新端点就绪，再从容器内执行 `capy ls` 验证接入。

为其他已有容器重复执行：

```sh
sudo capybarad attach --container wuxinyu --user wuxinyu --mount /home
sudo capybarad attach --container wuhuan --user wuhuan --mount /home
```

用户可以继续在原有 SSH 会话中使用：

```sh
capy nodes
capy submit -g 1 -p 10 -- python train.py
```

若当前 shell 缓存了旧命令路径，可以执行 `hash -r`；工具不会修改用户的 Conda 环境或 shell 配置。

## 4. 后续更新客户端

安装新的宿主机客户端后，重新执行对应的 `attach` 即可更新持久化目录里的副本：

```sh
sudo install -m 0755 bin/capy /usr/local/bin/capy
sudo capybarad attach --container luoyi --user luoyi --mount /home
```

重复操作会复用已有链接，不重复追加配置。客户端采用文件替换而非原地写入，已经运行的客户端进程继续使用旧文件，新启动的客户端使用新版；容器不用重启。

如果容器原来放置的是一个**普通客户端文件**，默认拒绝覆盖。确认需要替换后使用：

```sh
sudo capybarad attach --container luoyi --user luoyi --mount /home --replace-client
```

原文件会保留在容器内 `/usr/local/bin/capy.capybara-backup`。已有其他软链接、socket、直接绑定挂载的客户端或 socket 不会被强制覆盖；这些容器已有接入方式，应保留原方式或另行迁移。

## 5. 手动配置与热加载

自动接入工具等价于追加：

```yaml
additional_sockets:
  - /home/icca/users/luoyi/.capybara/capybara.sock
  - /home/icca/users/wuxinyu/.capybara/capybara.sock
users:
  luoyi: luoyi
  wuxinyu: wuxinyu
```

原有 `socket` 继续监听。可以手动修改 socket 路径、权限和用户映射，然后执行：

```sh
sudo systemctl reload capybarad
sudo journalctl -u capybarad -n 30 --no-pager
```

`reload` 只发送 SIGHUP，不取消作业、不停止调度器。日志应出现“socket 和用户映射热加载完成”。如果任一新端点冲突或配置无效，会记录错误并保留原有监听与用户映射。

`systemctl reload` 返回只表示信号已发送；自动接入工具还会验证容器内 CLI 可用。手动重载后请检查日志，尤其是变更失败时。

数据库目录、调度周期、Web 监听和其他非热加载配置仍需重启；如果与 socket 配置同时更改，SIGHUP 会拒绝整次变更，避免误以为全部配置已生效。不是 systemd 部署时，可使用 `--no-reload` 安装后自行向新版守护进程发送 SIGHUP。

## 限制与排查

- 自动接入当前支持已有 bind mount，不支持 Docker named volume；挂载源路径必须真实、规范，不能包含目录软链接。
- `.capybara` 必须由守护进程账号管理，不能允许组或其他用户写入；默认服务为 root。工具不会修改个人数据目录的所有权。
- Linux 文件路径 Unix socket 最长 107 字节，过长的挂载源会被拒绝。
- socket 是运行时文件，不是可以复制备份的普通文件；重启服务后由守护进程在持久化目录内重建。
- 不删除或替换 `.capybara` 目录。用户如果移动自己的挂载源或接入目录，需要重新检查路径。
- 用户名标签优先于配置映射；工具不会悄悄更改已有作业归属。
- 同一个服务的数据目录、日志目录和 Web 管理令牌不会复制到用户挂载目录中。
- 该方案解决容器接入与客户端升级；以后替换守护进程二进制仍需要计划服务重启。热加载不等于运行中的 Go 代码升级。

出现问题先查看：

```sh
sudo journalctl -u capybarad -n 50 --no-pager
docker exec luoyi /usr/local/bin/capy ls
```

目前已通过挂载识别、重复执行、配置保留、真实 Unix socket 凭据、热加载回滚和运行中作业不中断的回归测试；Docker 操作用替身测试，仍需在真实服务器上完成首次容器接入验收。
