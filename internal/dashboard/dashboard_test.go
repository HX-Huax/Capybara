package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capybara/internal/gpu"
	"capybara/internal/model"
	"capybara/internal/runner"
	"capybara/internal/scheduler"
	"capybara/internal/store"
)

const testToken = "test-dashboard-token-at-least-32-bytes"

func setup(t *testing.T) (http.Handler, *scheduler.Scheduler) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sched := scheduler.New(st, runner.Runner{DataDir: dir}, func(context.Context) ([]gpu.Device, error) {
		return []gpu.Device{{UUID: "GPU-test", TotalMiB: 8192}}, nil
	}, time.Hour)
	server, err := New(sched, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler(), sched
}
func call(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// 所有控制和数据接口均需认证，静态资源可用，令牌不得由查询参数代替。
func TestAuthenticationAndAssets(t *testing.T) {
	h, _ := setup(t)
	for _, route := range []struct{ method, path string }{{"GET", "/api/overview"}, {"GET", "/api/jobs/1"}, {"GET", "/api/jobs/1/logs"}, {"DELETE", "/api/jobs/1"}, {"PATCH", "/api/jobs/1/priority"}} {
		for _, token := range []string{"", "wrong"} {
			if w := call(h, route.method, route.path, token, ""); w.Code != 401 {
				t.Fatalf("未认证访问 %s 得到 %d", route.path, w.Code)
			}
		}
	}
	if w := call(h, "GET", "/api/overview?token="+testToken, "", ""); w.Code != 401 {
		t.Fatal("接受 URL 令牌")
	}
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w := call(h, "GET", path, "", "")
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("静态资源 %s: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), testToken) {
			t.Fatal("静态资源泄漏令牌")
		}
	}
	if w := call(h, "GET", "/missing", "", ""); w.Code != 404 {
		t.Fatal("未知路径应 404")
	}
	if _, err := New(nil, "short"); err == nil {
		t.Fatal("允许弱管理令牌")
	}
}

// 全局管理员能调整队列与取消，响应不包含环境密钥；日志按大小截尾。
func TestControlsAndBoundedLogs(t *testing.T) {
	h, s := setup(t)
	j := model.Job{User: "alice", Command: []string{"echo", "<script>alert(1)</script>"}, Env: map[string]string{"SECRET": "hidden-secret"}, NGPUs: 1}
	if err := s.Submit(&j); err != nil {
		t.Fatal(err)
	}
	w := call(h, "GET", "/api/overview", testToken, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "hidden-secret") {
		t.Fatalf("overview: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`{}`, `{"priority":101}`, `{"priority":1.5}`, `{"priority":2,"other":1}`, `{"priority":2} {}`} {
		if w = call(h, "PATCH", "/api/jobs/1/priority", testToken, body); w.Code != 400 {
			t.Fatalf("错误接受 %s: %d", body, w.Code)
		}
	}
	w = call(h, "PATCH", "/api/jobs/1/priority", testToken, `{"priority":20}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, err := s.Store.Get(j.ID)
	if err != nil || got.Priority != 20 {
		t.Fatalf("优先级未更新 %+v %v", got, err)
	}
	text := strings.Repeat("a", 100000) + "tail-marker"
	if err = os.WriteFile(s.Runner.LogPath(j.ID), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	w = call(h, "GET", "/api/jobs/1/logs", testToken, "")
	var logs struct {
		Text      string
		Truncated bool
	}
	if err = json.Unmarshal(w.Body.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	if !logs.Truncated || len(logs.Text) != 64<<10 || !strings.HasSuffix(logs.Text, "tail-marker") {
		t.Fatal("日志截尾错误")
	}
	if w = call(h, "DELETE", "/api/jobs/1", testToken, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ = s.Store.Get(1)
	if got.State != "cancelled" {
		t.Fatal(got.State)
	}
	if w = call(h, "PATCH", "/api/jobs/1/priority", testToken, `{"priority":1}`); w.Code != 409 {
		t.Fatal("允许修改终态作业")
	}
	if w = call(h, "GET", "/api/jobs/999", testToken, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	// 监控故障不能遮蔽队列，仍允许管理员查询与控制作业。
	s.Probe = func(context.Context) ([]gpu.Device, error) { return nil, errors.New("probe unavailable") }
	w = call(h, "GET", "/api/overview", testToken, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "probe unavailable") || !strings.Contains(w.Body.String(), "alice") {
		t.Fatal(w.Body.String())
	}
}
