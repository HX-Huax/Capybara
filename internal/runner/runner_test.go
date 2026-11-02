package runner

import (
	"capybara/internal/model"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 用本机 shell 执行包装脚本，验证恶意参数不会被再次解释。
func TestWrapperPreservesArguments(t *testing.T) {
	if _, e := exec.LookPath("setsid"); e != nil {
		t.Skip("缺少 setsid")
	}
	marker := filepath.Join(t.TempDir(), "injected")
	payload := "$(touch " + marker + ") ; echo bad"
	j := model.Job{ID: 987654321, UID: uint32(os.Getuid()), Cwd: t.TempDir(), Container: "container", Command: []string{"printf", "%s", payload}, Env: map[string]string{"CUDA_VISIBLE_DEVICES": "evil"}, GPUs: []string{"GPU-safe"}}
	a := Args(j)
	idx := -1
	for i, v := range a {
		if v == "container" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("缺少容器")
	}
	cmd := exec.Command(a[idx+1], a[idx+2:]...)
	cmd.Env = append(os.Environ(), "CAPYBARA_JOB_ID=987654321")
	out, e := cmd.CombinedOutput()
	defer os.Remove("/tmp/capybara/987654321.pid")
	if e != nil {
		t.Fatalf("%s: %v", out, e)
	}
	if string(out) != payload {
		t.Fatalf("参数发生变化: %q", out)
	}
	if _, e = os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("发生命令注入")
	}
	if strings.Contains(strings.Join(a, "\n"), "CUDA_VISIBLE_DEVICES=evil") {
		t.Fatal("用户覆盖 GPU 分配")
	}
}

// 替身 docker 模拟容器内取消失败，验证执行器报告资源不可释放。
func TestUnconfirmedCancellation(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *capy-stop*) exit 1;; esac\nexec sleep 30\n"
	if e := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if e := os.Mkdir(filepath.Join(dir, "logs"), 0700); e != nil {
		t.Fatal(e)
	}
	ctx, c := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer c()
	r := Runner{DataDir: dir, Grace: 20 * time.Millisecond}
	_, e := r.Run(ctx, model.Job{ID: 1, Command: []string{"true"}})
	if e == nil || !strings.Contains(e.Error(), ErrUnconfirmed.Error()) {
		t.Fatalf("应保留资源: %v", e)
	}
}
