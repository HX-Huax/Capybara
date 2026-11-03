// capybarad 提供 Unix socket API 与可选的本机 Web 面板，需要访问 Docker 与宿主机 /proc。
package main

import (
	"capybara/internal/api"
	"capybara/internal/config"
	"capybara/internal/dashboard"
	"capybara/internal/gpu"
	"capybara/internal/identity"
	"capybara/internal/localsocket"
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
	var e error
	if len(os.Args) > 1 && os.Args[1] == "attach" {
		e = runAttach(os.Args[2:])
	} else {
		e = run()
	}
	if e != nil {
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
	r := runner.Runner{DataDir: c.DataDir, Grace: c.CancelGrace}
	sched := scheduler.New(s, r, func(ctx context.Context) ([]gpu.Device, error) { return gpu.Probe(ctx, c.ExternalThresholdMiB) }, c.DefaultTimeLimit)
	if e = sched.Recover(); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	sockets := localsocket.New(identity.ConnContext)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sockets.Shutdown(cleanup)
	}()
	// 每个端点都是直接监听，不能通过代理转发丢失原始 SO_PEERCRED。
	mode, _ := c.Mode()
	if e = sockets.Apply(c.SocketPaths(), mode, api.Server{Scheduler: sched, Users: c.Users}.Handler()); e != nil {
		return e
	}
	var web *http.Server
	webErrors := make(chan error, 1)
	if c.Web.Listen != "" {
		panel, err := dashboard.New(sched, os.Getenv(c.Web.TokenEnv))
		if err != nil {
			return err
		}
		listener, err := net.Listen("tcp", c.Web.Listen)
		if err != nil {
			return fmt.Errorf("Web 监听失败: %w", err)
		}
		defer listener.Close()
		web = &http.Server{Handler: panel.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
		go func() { webErrors <- web.Serve(listener) }()
		slog.Info("Web 面板启动", "address", listener.Addr().String())
	}
	done := make(chan struct{})
	go func() { defer close(done); sched.Run(ctx, c.ScheduleInterval) }()
	slog.Info("服务启动", "sockets", c.SocketPaths())
	var serveErr error
serving:
	for {
		select {
		case <-ctx.Done():
			break serving
		case serveErr = <-sockets.Errors:
			break serving
		case serveErr = <-webErrors:
			break serving
		case <-hup:
			if *path == "" {
				slog.Error("热加载需要通过 --config 指定配置文件")
				continue
			}
			next, err := config.Load(*path)
			if err == nil {
				err = c.CheckReload(next)
			}
			if err == nil {
				nextMode, _ := next.Mode()
				err = sockets.Apply(next.SocketPaths(), nextMode, api.Server{Scheduler: sched, Users: next.Users}.Handler())
			}
			if err != nil {
				slog.Error("配置热加载失败，保留现有服务", "error", err)
				continue
			}
			c = next
			slog.Info("socket 和用户映射热加载完成", "sockets", c.SocketPaths())
		}
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = sockets.Shutdown(shutdown)
	if web != nil {
		_ = web.Shutdown(shutdown)
		_ = web.Close()
	}
	<-done
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}
