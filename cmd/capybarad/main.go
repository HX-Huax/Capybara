// capybarad 只监听本机 Unix socket；需要访问 Docker 与宿主机 /proc。
package main

import (
	"capybara/internal/api"
	"capybara/internal/config"
	"capybara/internal/gpu"
	"capybara/internal/identity"
	"capybara/internal/runner"
	"capybara/internal/scheduler"
	"capybara/internal/store"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if e := run(); e != nil {
		slog.Error("服务退出", "error", e)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "", "YAML 配置路径")
	flag.Parse()
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	for _, p := range []string{c.DataDir, filepath.Join(c.DataDir, "logs")} {
		if e = os.MkdirAll(p, 0700); e != nil {
			return e
		}
	}
	if e = os.Chmod(c.DataDir, 0700); e != nil {
		return e
	}
	// flock 保证只有一个守护进程操作该数据目录。锁文件的生命周期由内核管理。
	lock, e := os.OpenFile(filepath.Join(c.DataDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("数据目录已被使用: %w", e)
	}
	s, e := store.Open(filepath.Join(c.DataDir, "capybara.db"))
	if e != nil {
		return e
	}
	defer s.Close()
	// 活跃 socket 不可覆盖；只清理确认未被监听的旧 socket。
	if info, err := os.Lstat(c.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("socket 路径已存在且不是 socket")
		}
		conn, err := net.DialTimeout("unix", c.Socket, time.Second)
		if err == nil {
			conn.Close()
			return fmt.Errorf("socket 已在使用")
		}
		if !os.IsNotExist(err) && !isRefused(err) {
			return err
		}
		if e = os.Remove(c.Socket); e != nil {
			return e
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if e = os.MkdirAll(filepath.Dir(c.Socket), 0755); e != nil {
		return e
	}
	l, e := net.Listen("unix", c.Socket)
	if e != nil {
		return e
	}
	defer l.Close()
	mode, _ := c.Mode()
	if e = os.Chmod(c.Socket, mode); e != nil {
		return e
	}
	r := runner.Runner{DataDir: c.DataDir, Grace: c.CancelGrace}
	sched := scheduler.New(s, r, func(ctx context.Context) ([]gpu.Device, error) { return gpu.Probe(ctx, c.ExternalThresholdMiB) }, c.DefaultTimeLimit)
	if e = sched.Recover(); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); sched.Run(ctx, c.ScheduleInterval) }()
	srv := &http.Server{Handler: api.Server{Scheduler: sched, Users: c.Users}.Handler(), ConnContext: identity.ConnContext, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("服务启动", "socket", c.Socket)
	e = srv.Serve(l)
	stop()
	<-done
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}
func isRefused(e error) bool { return os.IsTimeout(e) == false && errors.Is(e, syscall.ECONNREFUSED) }
