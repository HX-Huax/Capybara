// Package runner 管理 docker exec 及容器内的进程组。
package runner

import (
	"capybara/internal/model"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrUnconfirmed 表示无法确认容器进程已终止，必须保留 GPU 分配。
var ErrUnconfirmed = errors.New("无法确认容器进程已终止")

// Runner 的目录由守护进程创建，作业输出文件仅允许服务账号读取。
type Runner struct {
	DataDir string
	Grace   time.Duration
}

// LogPath 使用数字 ID 构造路径，避免用户输入造成目录穿越。
func (r Runner) LogPath(id int64) string {
	return filepath.Join(r.DataDir, "logs", strconv.FormatInt(id, 10)+".log")
}

// Args 将真实命令保留为位置参数，shell 只解释固定的包装脚本。
func Args(j model.Job) []string {
	a := []string{"exec", "-u", strconv.FormatUint(uint64(j.UID), 10), "-w", j.Cwd}
	for k, v := range j.Env {
		if k != "CUDA_VISIBLE_DEVICES" && k != "CAPYBARA_JOB_ID" {
			a = append(a, "-e", k+"="+v)
		}
	}
	a = append(a, "-e", "CUDA_VISIBLE_DEVICES="+strings.Join(j.GPUs, ","), "-e", fmt.Sprintf("CAPYBARA_JOB_ID=%d", j.ID), j.Container, "sh", "-c", `command -v setsid >/dev/null || exit 127
mkdir -p /tmp/capybara || exit 125
exec setsid sh -c 'umask 077; echo $$ > /tmp/capybara/$CAPYBARA_JOB_ID.pid; exec "$@"' capy-job "$@"`, "capy-wrapper")
	return append(a, j.Command...)
}

// Run 取消时先终止容器内的进程组，再回收宿主机 docker 客户端。
func (r Runner) Run(ctx context.Context, j model.Job) (int, error) {
	f, e := os.OpenFile(r.LogPath(j.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return -1, e
	}
	defer f.Close()
	cmd := exec.Command("docker", Args(j)...)
	cmd.Stdout = f
	cmd.Stderr = f
	if e = cmd.Start(); e != nil {
		return -1, e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case e = <-done:
	case <-ctx.Done():
		// 固定脚本仅接收数字作业 ID，信号使用位置参数传递。
		stop := func(signal string) error {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return exec.CommandContext(c, "docker", "exec", j.Container, "sh", "-c", `p=$(cat /tmp/capybara/"$1".pid) || exit 1; case "$p" in ''|*[!0-9]*) exit 1;; esac; kill -"$2" -- -"$p"`, "capy-stop", strconv.FormatInt(j.ID, 10), signal).Run()
		}
		// 包装脚本可能尚未写出 PID；重试直至进程退出或宽限期结束。
		timer := time.NewTimer(r.Grace)
		defer timer.Stop()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			_ = stop("TERM")
			select {
			case e = <-done:
				return cmd.ProcessState.ExitCode(), e
			case <-ticker.C:
				continue
			case <-timer.C:
				if err := stop("KILL"); err != nil {
					_ = cmd.Process.Kill()
					<-done
					return -1, fmt.Errorf("%w: %v", ErrUnconfirmed, err)
				}
				_ = cmd.Process.Kill()
				e = <-done
				return -1, e
			}
		}
	}
	return cmd.ProcessState.ExitCode(), e
}
