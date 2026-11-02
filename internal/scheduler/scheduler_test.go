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
