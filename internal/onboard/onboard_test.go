package onboard

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"capybara/internal/config"
	"capybara/internal/localsocket"
)

func inspectFixture(source string) []byte {
	data := []map[string]any{{"Id": strings.Repeat("a", 64), "Name": "/alice", "State": map[string]bool{"Running": true}, "Config": map[string]any{"Labels": map[string]string{}}, "Mounts": []map[string]string{{"Type": "bind", "Source": source, "Destination": "/home"}}}}
	b, _ := json.Marshal(data)
	return b
}
func TestPlanAndMountValidation(t *testing.T) {
	o := Options{User: "alice", Mount: "/home"}
	p, err := BuildPlan(inspectFixture("/srv/alice"), o)
	if err != nil {
		t.Fatal(err)
	}
	if p.HostDir != "/srv/alice/.capybara" || p.ContainerDir != "/home/.capybara" || p.ContainerName != "alice" {
		t.Fatalf("错误计划: %+v", p)
	}
	for _, data := range []string{
		strings.ReplaceAll(string(inspectFixture("/srv/alice")), `"Running":true`, `"Running":false`),
		strings.ReplaceAll(string(inspectFixture("/srv/alice")), `"Type":"bind"`, `"Type":"volume"`),
		strings.ReplaceAll(string(inspectFixture("/srv/alice")), `"Labels":{}`, `"Labels":{"capybara.user":"bob"}`),
		strings.ReplaceAll(string(inspectFixture("/srv/alice")), `"Destination":"/home"`, `"Destination":"/workspace"`),
	} {
		if _, err = BuildPlan([]byte(data), o); err == nil {
			t.Fatalf("接受无效容器配置: %s", data)
		}
	}
}

// YAML 注释、桌面显存阈值与 Web 设置必须保留，重复接入不能重复追加端点。
func TestConfigUpdatePreservesSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	before := []byte("# lab config\nexternal_use_threshold_mib: 1024 # desktop\nusers: null\nadditional_sockets: null\nweb:\n  listen: 127.0.0.1:8080\n")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := Plan{ContainerName: "alice", User: "alice", Socket: "/srv/alice/.capybara/capybara.sock"}
	after, err := updateConfig(before, p, c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte("# lab config")) || !bytes.Contains(after, []byte("# desktop")) {
		t.Fatal(string(after))
	}
	if err = os.WriteFile(path, after, 0600); err != nil {
		t.Fatal(err)
	}
	next, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CheckReload(next); err != nil {
		t.Fatal(err)
	}
	if next.Users["alice"] != "alice" || len(next.AdditionalSockets) != 1 || next.ExternalThresholdMiB != 1024 || next.Web.Listen != "127.0.0.1:8080" {
		t.Fatalf("设置丢失: %+v", next)
	}
	twice, err := updateConfig(after, p, next)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, twice) {
		t.Fatalf("重复操作改变配置:\n%s", twice)
	}
}

func TestAtomicClientReplacement(t *testing.T) {
	root := t.TempDir()
	dir, err := localsocket.Directory(filepath.Join(root, "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	target := filepath.Join(root, "bundle", "capy")
	if err = atomicCopy(dir, "capy", strings.NewReader("old"), 0755); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err = atomicCopy(dir, "capy", strings.NewReader("new"), 0755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "new" {
		t.Fatal("客户端未更新")
	}
	old := make([]byte, 3)
	if _, err = held.Read(old); err != nil || string(old) != "old" {
		t.Fatal("破坏了旧文件打开句柄")
	}
}

// Docker 替身只确认参数和挂载探针，不对测试宿主机执行容器内的绝对路径操作。
func TestAttachNoReloadAndIdempotence(t *testing.T) {
	root, err := os.MkdirTemp("", "capy-onboard-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	source := filepath.Join(root, "home")
	if err = os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(root, "inspect.json")
	if err = os.WriteFile(fixture, inspectFixture(source), 0600); err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(root, "client")
	if err = os.WriteFile(client, []byte("#!/bin/sh\necho client\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "config.yaml")
	before := []byte("# keep\nexternal_use_threshold_mib: 1024\n")
	if err = os.WriteFile(cfg, before, 0600); err != nil {
		t.Fatal(err)
	}
	docker := `#!/usr/bin/python3
import os,sys,json
args=sys.argv[1:]
if args[0]=='inspect':
 print(open(os.environ['ATTACH_FIXTURE']).read())
elif args[0]=='exec':
 if '.attach-probe-' in ' '.join(args):
  marker=args[-1];probe=args[-2]
  assert open(os.path.join(os.environ['ATTACH_SOURCE'],'.capybara',probe)).read()==marker
 with open(os.environ['ATTACH_CALLS'],'a') as f:f.write(json.dumps(args)+'\n')
else:sys.exit(1)
`
	if err = os.WriteFile(filepath.Join(root, "docker"), []byte(docker), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("ATTACH_FIXTURE", fixture)
	t.Setenv("ATTACH_SOURCE", source)
	t.Setenv("ATTACH_CALLS", filepath.Join(root, "calls"))
	o := Options{Container: "alice", User: "alice", Mount: "/home", Client: client, Config: cfg, NoReload: true}
	var out bytes.Buffer
	for i := 0; i < 2; i++ {
		if err = Run(context.Background(), o, &out); err != nil {
			t.Fatal(err)
		}
	}
	c, err := config.Load(cfg)
	if err != nil || len(c.AdditionalSockets) != 1 || c.Users["alice"] != "alice" || c.ExternalThresholdMiB != 1024 {
		t.Fatalf("配置未正确更新: %+v %v", c, err)
	}
	b, err := os.ReadFile(filepath.Join(source, ".capybara", "capy"))
	if err != nil || !bytes.Contains(b, []byte("echo client")) {
		t.Fatal("未安装客户端")
	}
	backup, err := os.ReadFile(cfg + ".before-attach")
	if err != nil || !bytes.Equal(backup, before) {
		t.Fatal("原配置备份未保留")
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil || bytes.Contains(calls, []byte(`"restart"`)) || bytes.Contains(calls, []byte(`"stop"`)) {
		t.Fatal("不应停止或重启容器")
	}
}

// 在临时目录替换脚本中的固定系统路径，实际执行链接/备份逻辑，不触碰宿主机系统目录。
func TestContainerLinkScriptConflictsAndBackup(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	bin := filepath.Join(root, "bin")
	run := filepath.Join(root, "run")
	for _, dir := range []string{bundle, bin, run} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	client := filepath.Join(bin, "capy")
	if err := os.WriteFile(client, []byte("old-client"), 0755); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(linkScript, "/usr/local/bin", bin)
	script = strings.ReplaceAll(script, "/run/capybara", run)
	execute := func(replace string) error {
		cmd := exec.Command("sh", "-c", script, "capy-test", bundle, replace, "1")
		return cmd.Run()
	}
	if err := execute("0"); err == nil {
		t.Fatal("默认覆盖了原客户端")
	}
	if err := execute("1"); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(client + ".capybara-backup")
	if err != nil || string(old) != "old-client" {
		t.Fatal("未保留原客户端")
	}
	if target, err := os.Readlink(client); err != nil || target != filepath.Join(bundle, "capy") {
		t.Fatal("客户端链接错误")
	}
	if err = execute("1"); err != nil {
		t.Fatal("重复接入应成功")
	}
	if err = os.Remove(client); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("/unknown/client", client); err != nil {
		t.Fatal(err)
	}
	if err = execute("1"); err == nil {
		t.Fatal("覆盖了未知软链接")
	}
	if target, _ := os.Readlink(client); target != "/unknown/client" {
		t.Fatal("未知链接被修改")
	}
}
