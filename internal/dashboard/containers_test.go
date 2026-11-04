package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"capybara/internal/model"
)

// 用 Docker 替身验证参数与失败路径，测试不接触宿主机的实际容器。
func TestContainerRestart(t *testing.T) {
	h, s := setup(t)
	dir := t.TempDir()
	id := strings.Repeat("a", 64)
	t.Setenv("DOCKER_TEST_DIR", dir)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	script := "#!/bin/sh\nif [ \"$1\" = ps ]; then\n echo '{\"ID\":\"" + id + "\",\"Names\":\"wangyang\",\"Image\":\"ubuntu:24.04\",\"State\":\"running\",\"Status\":\"Up 1 hour\"}'\nelse\n printf '%s\\n' \"$@\" > \"$DOCKER_TEST_DIR/args\"\n test ! -f \"$DOCKER_TEST_DIR/fail\"\nfi\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	path := "/api/containers/" + id + "/restart"
	for _, route := range []struct{ method, path string }{{"GET", "/api/containers"}, {"POST", path}} {
		if w := call(h, route.method, route.path, "", `{"mode":"force"}`); w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	if w := call(h, "GET", "/api/containers", testToken, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "wangyang") {
		t.Fatal(w.Body.String())
	}
	for _, body := range []string{`{}`, `{"mode":"kill"}`, `{"mode":"force","extra":1}`, `{"mode":"force"} {}`} {
		if w := call(h, "POST", path, testToken, body); w.Code != 400 {
			t.Fatal(w.Code, body)
		}
	}
	if w := call(h, "POST", "/api/containers/--all/restart", testToken, `{"mode":"force"}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call(h, "POST", "/api/containers/"+strings.Repeat("b", 64)+"/restart", testToken, `{"mode":"force"}`); w.Code != 404 {
		t.Fatal(w.Code)
	}
	for _, mode := range []struct{ name, timeout string }{{"graceful", "10"}, {"force", "0"}} {
		if w := call(h, "POST", path, testToken, `{"mode":"`+mode.name+`"}`); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		args, err := os.ReadFile(filepath.Join(dir, "args"))
		if err != nil {
			t.Fatal(err)
		}
		if string(args) != "restart\n--timeout\n"+mode.timeout+"\n"+id+"\n" {
			t.Fatal(string(args))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "POST", path, testToken, `{"mode":"force"}`); w.Code != 502 {
		t.Fatal(w.Code)
	}
	if s.RestartStates()[id] != "restart_error" {
		t.Fatal("失败未保留维护状态")
	}
	if err := os.Remove(filepath.Join(dir, "fail")); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "POST", path, testToken, `{"mode":"graceful"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(s.RestartStates()) != 0 {
		t.Fatal("重试成功未恢复调度")
	}
	j := model.Job{User: "test", Container: id}
	if err := s.Submit(&j); err != nil {
		t.Fatal(err)
	}
	j.State = "running"
	if err := s.Store.Transition(j, "pending"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"force", "graceful"} {
		if w := call(h, "POST", path, testToken, `{"mode":"`+mode+`"}`); w.Code != 409 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
