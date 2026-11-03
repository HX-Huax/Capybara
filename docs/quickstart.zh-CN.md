# Capybara 快速使用

适用于已经接入 Capybara 的 Docker 容器。尚未接入时，先阅读[已有容器接入与升级](existing-containers.zh-CN.md)。

## 进入容器

在宿主机执行：

```bash
docker exec -it wangyang /bin/bash
```

以下命令均在容器内执行。切换到实际项目目录，提交时会记录当前工作目录：

```bash
cd /workspace/你的项目目录
```

## 提交作业

```bash
# 申请 1 张 GPU，最多运行 2 小时
capy submit -g 1 -t 2h -- python -u train.py

# 指定优先级：范围 -100～100，数值越大越优先，默认 0
capy submit -g 1 -p 20 -t 2h -- python -u train.py

# 不申请 GPU 的 CPU 作业
capy submit -g 0 -- bash test.sh
```

提交成功会返回 JSON，记下其中的 `id`。以下示例使用作业编号 `12`，请替换为实际编号。

`--` 后面的内容是要执行的命令和参数。需要管道或重定向等 shell 功能时，显式使用 `sh -c`：

```bash
capy submit -g 1 -- sh -c 'python -u train.py | tee training.log'
```

优先级相同时按提交顺序调度，不抢占已经运行的作业。队首 GPU 不足时，后面的作业也会等待。`-t 2h` 从开始运行时计时，不包含排队时间。

关闭 SSH 连接后，已提交的作业仍由守护进程管理，无需额外使用 `nohup`。

## 使用 Conda 和环境变量

当前客户端不会自动复制激活 Conda 后的全部环境变量。建议指定环境中 Python 的完整路径：

```bash
capy submit -g 1 -- \
  /你的conda目录/envs/环境名/bin/python -u train.py
```

在已激活的环境中执行 `which python` 可以查看实际路径。程序需要的其他环境变量通过 `--env` 显式传递：

```bash
# 传递当前终端已设置的变量
capy submit -g 1 --env PYTHONPATH -- python -u train.py

# 直接指定变量值，可重复使用 --env
capy submit -g 1 --env EXPERIMENT=baseline -- python -u train.py
```

GPU 可见性由服务端设置，不能通过 `--env CUDA_VISIBLE_DEVICES=...` 覆盖分配结果。

## 查看标准输出和错误输出

```bash
# 查看已有输出
capy logs 12

# 实时跟随输出
capy logs -f 12
```

程序的 **stdout 和 stderr 合并保存在同一份作业日志中**。程序结束后仍可查询。排队中的任务尚未执行，因此日志可能为空；程序主动重定向到其他文件的内容不会自动出现在这里。

Python 建议使用 `python -u` 减少输出缓冲，使 `print()` 更及时地显示，也可以在代码中显式刷新：

```python
# 及时刷新进度，便于从 capy logs -f 观察运行情况。
print("当前训练进度", flush=True)
```

在 `capy logs -f` 中按 **Ctrl+C 只退出日志查看，不取消作业**。终止作业需要执行 `capy cancel`。

将已有日志保存到当前目录：

```bash
capy logs 12 > job-12.log
```

Web 面板中点击“作业详情 → 执行日志”也能查看输出。面板显示末尾 **64 KiB**，默认每 3 秒刷新；完整日志使用 `capy logs 12` 获取。

## 常用管理命令

| 操作 | 命令 |
| --- | --- |
| 查看自己的作业 | `capy ls` |
| 查看详情、状态和退出码 | `capy info 12` |
| 查看已有日志 | `capy logs 12` |
| 实时跟随日志 | `capy logs -f 12` |
| 查看 GPU 状态 | `capy nodes` |
| 取消排队或运行中的作业 | `capy cancel 12` |

取消运行作业需要等待进程终止后才会更新为终态，可以再次执行 `capy info 12` 确认。

| 状态 | 含义 |
| --- | --- |
| `pending` | 排队中 |
| `running` | 正在运行；异常重启后也可能表示保留分配、等待人工确认 |
| `succeeded` | 成功结束 |
| `failed` | 执行失败，检查退出码和日志 |
| `timeout` | 超过运行时限并结束 |
| `cancelled` | 已取消 |
| `lost` | 状态模型中的失联终态；当前版本尚未实现自动恢复判定 |

## 最小验证流程

```bash
capy submit -g 0 -- sh -c 'echo hello-capybara; id; pwd'
capy ls

# 将 12 替换为刚才返回的作业编号
capy info 12
capy logs 12
```

预期作业最终为 `succeeded`，`exit_code` 为 `0`，日志包含 `hello-capybara`。

如果 GPU 作业一直排队，先运行 `capy nodes`：`external: true` 表示显存阈值检测认为存在外部占用，`job_id` 非零表示已有内部作业分配。日志为空时，先用 `capy info` 确认作业是否已经启动。

当前版本正常停止守护进程会取消活跃作业，异常退出后还不能自动接管；请与管理员协调服务升级时间。
