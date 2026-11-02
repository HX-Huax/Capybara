package api

import (
	"capybara/internal/identity"
	"capybara/internal/model"
	"capybara/internal/runner"
	"capybara/internal/scheduler"
	"capybara/internal/store"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 验证归属校验发生在日志读取/取消之前，详情不会泄漏环境密钥。
func TestJobOwnershipAndRedaction(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	s := Server{Scheduler: scheduler.New(st, runner.Runner{}, nil, time.Hour)}
	j := model.Job{User: "alice", Env: map[string]string{"SECRET": "private-value"}}
	if e = st.Create(&j); e != nil {
		t.Fatal(e)
	}
	for _, user := range []string{"alice", "bob"} {
		r := httptest.NewRequest("GET", "/v1/jobs/1", nil)
		r.SetPathValue("id", "1")
		r = r.WithContext(context.WithValue(r.Context(), identityKey{}, identity.Identity{User: user}))
		w := httptest.NewRecorder()
		s.job(w, r)
		want := http.StatusOK
		if user == "bob" {
			want = http.StatusForbidden
		}
		if w.Code != want {
			t.Fatalf("%s: %d", user, w.Code)
		}
		if strings.Contains(w.Body.String(), "private-value") {
			t.Fatal("详情泄漏环境变量")
		}
	}
	r := httptest.NewRequest("DELETE", "/v1/jobs/1", nil)
	r.SetPathValue("id", "1")
	r = r.WithContext(context.WithValue(r.Context(), identityKey{}, identity.Identity{User: "bob"}))
	w := httptest.NewRecorder()
	s.job(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	got, _ := st.Get(1)
	if got.State != "pending" {
		t.Fatal("其他用户修改了作业")
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/jobs", nil))
	if w.Code != 403 {
		t.Fatal("缺少内核凭据的请求应拒绝")
	}
}
