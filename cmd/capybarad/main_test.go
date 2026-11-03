package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"capybara/internal/model"
	"capybara/internal/store"
)

// 独立子进程运行真实服务，避免 SIGHUP 影响测试进程本身。
func TestDaemonHelperProcess(t *testing.T) {
	if os.Getenv("CAPY_TEST_DAEMON") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("daemon-test", flag.ContinueOnError)
	os.Args = []string{os.Args[0], "--config", os.Getenv("CAPY_TEST_CONFIG")}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestReloadPreservesRunningJobAndOldEndpoints(t *testing.T) {
	root, err := os.MkdirTemp("", "capy-reload-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	data := filepath.Join(root, "data")
	if err = os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(data, "capybara.db"))
	if err != nil {
		t.Fatal(err)
	}
	job := model.Job{User: "alice", Container: strings.Repeat("a", 64), Command: []string{"sleep", "3"}, NGPUs: 0, TimeLimit: 10 * time.Second}
	if err = database.Create(&job); err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for name, body := range map[string]string{
		"docker":     "#!/bin/sh\ncase \"$*\" in *capy-stop*) exit 0;; esac\nexec sleep 3\n",
		"nvidia-smi": "#!/bin/sh\necho 'GPU-test, 0, 0, 8192, 0, Test GPU'\n",
	} {
		if err = os.WriteFile(filepath.Join(root, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	primary := filepath.Join(root, "run", "main.sock")
	alias := filepath.Join(root, "bundle", "capybara.sock")
	cfg := filepath.Join(root, "config.yaml")
	writeConfig := func(extra string) {
		t.Helper()
		text := fmt.Sprintf("socket: %s\ndata_dir: %s\nschedule_interval: 20ms\ncancel_grace: 100ms\n", primary, data)
		if extra != "" {
			text += "additional_sockets:\n  - " + extra + "\nusers:\n  alice: alice\n"
		}
		if err = os.WriteFile(cfg, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("")
	logFile, err := os.Create(filepath.Join(root, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonHelperProcess$")
	cmd.Env = append(os.Environ(), "CAPY_TEST_DAEMON=1", "CAPY_TEST_CONFIG="+cfg, "PATH="+root+":"+os.Getenv("PATH"))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}()
	wait := func(f func() bool) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			if f() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		b, _ := os.ReadFile(logFile.Name())
		t.Fatalf("等待超时:\n%s", b)
	}
	wait(func() bool { j, e := database.Get(job.ID); return e == nil && j.State == "running" })
	before, _ := database.Get(job.ID)
	writeConfig(alias)
	if err = cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { _, e := os.Stat(alias); return e == nil })
	// 宿主机请求应被身份校验拒绝，新增监听不能绕过安全边界。
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", alias)
	}}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := client.Get("http://capy/v1/jobs")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	transport.CloseIdleConnections()
	if response.StatusCode != 403 {
		t.Fatal(response.StatusCode)
	}
	after, _ := database.Get(job.ID)
	if after.State != "running" || !after.StartAt.Equal(*before.StartAt) {
		t.Fatal("热加载打断了运行作业")
	}
	conflict := filepath.Join(root, "bundle", "conflict.sock")
	if err = os.WriteFile(conflict, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	writeConfig(conflict)
	if err = cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	wait(func() bool {
		b, _ := os.ReadFile(logFile.Name())
		return strings.Contains(string(b), "配置热加载失败")
	})
	if _, err = os.Stat(alias); err != nil {
		t.Fatal("失败热加载破坏了旧端点")
	}
	wait(func() bool { j, e := database.Get(job.ID); return e == nil && j.State == "succeeded" })
}
