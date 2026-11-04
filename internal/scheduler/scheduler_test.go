package scheduler

import (
	"capybara/internal/gpu"
	"capybara/internal/model"
	"capybara/internal/runner"
	"capybara/internal/store"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 大作业放不下时，小作业不得绕过队首；取消队首后应继续运行。
func TestFIFOAndCompletion(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\necho completed\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if e := os.Mkdir(filepath.Join(dir, "logs"), 0700); e != nil {
		t.Fatal(e)
	}
	st, e := store.Open(filepath.Join(dir, "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	s := New(st, runner.Runner{DataDir: dir, Grace: time.Second}, func(context.Context) ([]gpu.Device, error) { return []gpu.Device{{UUID: "GPU-1"}}, nil }, time.Second)
	a := model.Job{User: "a", NGPUs: 2}
	b := model.Job{User: "b", NGPUs: 1}
	if e = s.Submit(&a); e != nil {
		t.Fatal(e)
	}
	if e = s.Submit(&b); e != nil {
		t.Fatal(e)
	}
	s.tick(context.Background())
	got, _ := st.Get(b.ID)
	if got.State != "pending" {
		t.Fatal("发生队首绕过")
	}
	if e = s.Cancel(a.ID); e != nil {
		t.Fatal(e)
	}
	s.tick(context.Background())
	s.wg.Wait()
	got, e = st.Get(b.ID)
	if e != nil || got.State != "succeeded" {
		t.Fatalf("未完成: %+v %v", got, e)
	}
	if e = s.Cancel(b.ID); e != nil {
		t.Fatal(e)
	}
}

// 高优先级队首阻塞时，低优先级的小作业不能绕过；调整后队列立即生效。
func TestPriorityOrderingAndUpdates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := os.Mkdir(filepath.Join(dir, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "jobs.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, runner.Runner{DataDir: dir}, func(context.Context) ([]gpu.Device, error) { return []gpu.Device{{UUID: "GPU-1"}}, nil }, time.Second)
	low := model.Job{User: "alice", NGPUs: 1, Priority: -10}
	high := model.Job{User: "bob", NGPUs: 2, Priority: 20}
	for _, j := range []*model.Job{&low, &high} {
		if err = s.Submit(j); err != nil {
			t.Fatal(err)
		}
	}
	s.tick(context.Background())
	got, _ := st.Get(low.ID)
	if got.State != "pending" {
		t.Fatal("低优先级作业绕过队首")
	}
	if _, err = s.SetPriority(low.ID, 30); err != nil {
		t.Fatal(err)
	}
	// 重新打开数据库模拟服务重启，新的优先级必须保持。
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s.Store = st
	got, err = st.Get(low.ID)
	if err != nil || got.Priority != 30 {
		t.Fatalf("优先级未持久化: %+v %v", got, err)
	}
	s.tick(context.Background())
	s.wg.Wait()
	got, _ = st.Get(low.ID)
	if got.State != "succeeded" {
		t.Fatalf("优先级调整后未执行: %+v", got)
	}
	got, _ = st.Get(high.ID)
	if got.State != "pending" {
		t.Fatal("大作业应继续等待")
	}
	if _, err = s.SetPriority(low.ID, 40); err == nil {
		t.Fatal("允许修改已结束作业")
	}
	if _, err = s.SetPriority(high.ID, 101); err == nil {
		t.Fatal("允许越界优先级")
	}
}

// 维护中的容器跳过调度，但不会阻塞其他容器；失败后需重试成功才恢复。
func TestRestartSchedulingGuard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := os.Mkdir(filepath.Join(dir, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(st, runner.Runner{DataDir: dir}, func(context.Context) ([]gpu.Device, error) { return nil, nil }, time.Second)
	a, b := model.Job{User: "a", Container: "a"}, model.Job{User: "b", Container: "b"}
	for _, j := range []*model.Job{&a, &b} {
		if err = s.Submit(j); err != nil {
			t.Fatal(err)
		}
	}
	finish, err := s.BeginRestart("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginRestart("a"); err == nil {
		t.Fatal("允许并发重启")
	}
	s.tick(context.Background())
	s.wg.Wait()
	ja, _ := st.Get(a.ID)
	jb, _ := st.Get(b.ID)
	if ja.State != "pending" || jb.State != "succeeded" {
		t.Fatal(ja.State, jb.State)
	}
	finish(false)
	s.tick(context.Background())
	s.wg.Wait()
	ja, _ = st.Get(a.ID)
	if ja.State != "pending" {
		t.Fatal("失败后仍然调度")
	}
	finish, err = s.BeginRestart("a")
	if err != nil {
		t.Fatal(err)
	}
	finish(true)
	s.tick(context.Background())
	s.wg.Wait()
	ja, _ = st.Get(a.ID)
	if ja.State != "succeeded" {
		t.Fatal(ja.State)
	}
}
