package store

import (
	"capybara/internal/model"
	"path/filepath"
	"testing"
	"time"
)

// 验证数据库重开后的持久化，以及分配冲突是否整体回滚。
func TestAllocationTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	a := model.Job{User: "alice", Command: []string{"true"}, Env: map[string]string{"SECRET": "value"}}
	b := a
	if e = s.Create(&a); e != nil {
		t.Fatal(e)
	}
	if e = s.Create(&b); e != nil {
		t.Fatal(e)
	}
	a.State = "running"
	a.GPUs = []string{"GPU-1"}
	if e = s.Transition(a, "pending"); e != nil {
		t.Fatal(e)
	}
	b.State = "running"
	b.GPUs = a.GPUs
	if e = s.Transition(b, "pending"); e == nil {
		t.Fatal("重复分配应失败")
	}
	got, e := s.Get(b.ID)
	if e != nil || got.State != "pending" {
		t.Fatalf("事务未回滚: %+v %v", got, e)
	}
	a.State = "succeeded"
	now := time.Now()
	a.EndAt = &now
	if e = s.Transition(a, "running"); e != nil {
		t.Fatal(e)
	}
	if e = s.Transition(b, "pending"); e != nil {
		t.Fatal(e)
	}
	if e = s.Transition(b, "pending"); e == nil {
		t.Fatal("过期状态转换应失败")
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	got, e = s.Get(b.ID)
	if e != nil || got.State != "running" || got.Env["SECRET"] != "value" {
		t.Fatalf("恢复失败: %+v %v", got, e)
	}
}
