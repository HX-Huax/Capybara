// Package onboard 将已有 Docker bind mount 用于客户端与 socket，不重建或重启容器。
package onboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"capybara/internal/config"
	"capybara/internal/localsocket"
	"gopkg.in/yaml.v3"
)

// Options 指定现有容器与已挂载目录；不会自动安装容器依赖或覆盖未知文件。
type Options struct {
	Container, User, Mount, Client, Config string
	ReplaceClient, NoReload, DryRun        bool
}

// Plan 只包含路径和身份，不保存 docker inspect 中可能存在的环境密钥。
type Plan struct{ ContainerID, ContainerName, User, HostDir, ContainerDir, Socket string }

type inspection struct {
	ID     string `json:"Id"`
	Name   string
	State  struct{ Running bool }
	Config struct{ Labels map[string]string }
	Mounts []struct{ Type, Source, Destination string }
}

// BuildPlan 从 Docker 返回的实际挂载中选择接入位置，不猜测宿主机目录。
func BuildPlan(data []byte, o Options) (Plan, error) {
	var rows []inspection
	if err := json.Unmarshal(data, &rows); err != nil {
		return Plan{}, err
	}
	if len(rows) != 1 || !rows[0].State.Running {
		return Plan{}, fmt.Errorf("目标容器必须正在运行")
	}
	row := rows[0]
	if len(row.ID) != 64 || strings.Trim(row.ID, "0123456789abcdef") != "" {
		return Plan{}, fmt.Errorf("无效容器 ID")
	}
	if o.User == "" || strings.ContainsAny(o.User, "\x00\r\n") {
		return Plan{}, fmt.Errorf("必须指定有效 --user")
	}
	if label := row.Config.Labels["capybara.user"]; label != "" && label != o.User {
		return Plan{}, fmt.Errorf("容器用户标签为 %q，与指定用户不同", label)
	}
	if !filepath.IsAbs(o.Mount) || filepath.Clean(o.Mount) != o.Mount {
		return Plan{}, fmt.Errorf("--mount 必须为容器内规范绝对路径")
	}
	var source string
	for _, m := range row.Mounts {
		if m.Destination == o.Mount {
			if m.Type != "bind" {
				return Plan{}, fmt.Errorf("当前接入工具要求已有 bind mount")
			}
			source = m.Source
		}
	}
	if source == "" || !filepath.IsAbs(source) || filepath.Clean(source) != source {
		return Plan{}, fmt.Errorf("未找到挂载到 %s 的真实 bind mount", o.Mount)
	}
	inside := filepath.Join(o.Mount, ".capybara")
	for _, m := range row.Mounts {
		if m.Destination == "/usr/local/bin/capy" || m.Destination == "/run/capybara" || m.Destination == "/run/capybara/capybara.sock" {
			return Plan{}, fmt.Errorf("容器已有直接接入挂载 %s，无需使用 attach", m.Destination)
		}
		if m.Destination != o.Mount && (m.Destination == inside || strings.HasPrefix(inside, m.Destination+"/") && strings.HasPrefix(m.Destination, o.Mount+"/") || strings.HasPrefix(m.Destination, inside+"/")) {
			return Plan{}, fmt.Errorf("接入目录被其他挂载遮蔽: %s", m.Destination)
		}
	}
	host := filepath.Join(source, ".capybara")
	socket := filepath.Join(host, "capybara.sock")
	if len(socket) > 107 {
		return Plan{}, fmt.Errorf("socket 路径超过 Unix socket 的 107 字节限制")
	}
	return Plan{row.ID, strings.TrimPrefix(row.Name, "/"), o.User, host, inside, socket}, nil
}

func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	child, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(child, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s 失败: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}
func dockerScript(ctx context.Context, p Plan, script string, args ...string) error {
	argv := []string{"exec", "--user", "0", p.ContainerID, "sh", "-c", script, "capy-attach"}
	argv = append(argv, args...)
	_, err := command(ctx, "docker", argv...)
	return err
}

// 脚本只解释固定内容；目录与选项作为位置参数传入，避免 shell 注入。
const linkScript = `set -eu
bundle=$1
replace=$2
apply=$3
fail() { echo "$1" >&2; exit 1; }
for tool in setsid readlink ln mkdir mv cat; do command -v "$tool" >/dev/null || fail "容器缺少依赖: $tool"; done
check_link() {
 path=$1
 target=$2
 if [ -L "$path" ]; then
  [ "$(readlink "$path")" = "$target" ] || fail "已有不同的软链接，拒绝覆盖: $path"
 elif [ -e "$path" ]; then
  if [ "$path" = /usr/local/bin/capy ] && [ "$replace" = 1 ] && [ -f "$path" ]; then
   [ ! -e "$path.capybara-backup" ] && [ ! -L "$path.capybara-backup" ] || fail "客户端备份已存在，拒绝覆盖"
  else fail "已有文件或 socket，拒绝覆盖: $path"; fi
 fi
}
# 不改写已有目录链接或已有 socket 挂载。
[ ! -L /run/capybara ] || fail "/run/capybara 已是软链接，请人工检查"
[ ! -e /run/capybara ] || [ -d /run/capybara ] || fail "/run/capybara 不是目录"
check_link /usr/local/bin/capy "$bundle/capy"
check_link /run/capybara/capybara.sock "$bundle/capybara.sock"
if [ "$apply" = 1 ]; then
 mkdir -p /usr/local/bin /run/capybara
 if [ -f /usr/local/bin/capy ] && [ ! -L /usr/local/bin/capy ]; then mv /usr/local/bin/capy /usr/local/bin/capy.capybara-backup; fi
 [ -L /usr/local/bin/capy ] || ln -s "$bundle/capy" /usr/local/bin/capy
 [ -L /run/capybara/capybara.sock ] || ln -s "$bundle/capybara.sock" /run/capybara/capybara.sock
fi
`

// Run 先检查全部前置条件，再原子更新客户端与 YAML，最后请求服务热加载。
// 中途失败不删除实验数据或重启容器；再次运行会复用已安装的链接和配置。
func Run(ctx context.Context, o Options, out io.Writer) error {
	raw, err := command(ctx, "docker", "inspect", "--type", "container", "--", o.Container)
	if err != nil {
		return err
	}
	plan, err := BuildPlan(raw, o)
	if err != nil {
		return err
	}
	client, err := os.Open(o.Client)
	if err != nil {
		return err
	}
	defer client.Close()
	info, err := client.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("--client 必须是可执行的普通文件")
	}
	cfgPath, err := filepath.Abs(o.Config)
	if err != nil {
		return err
	}
	cfgDir, err := localsocket.Directory(filepath.Dir(cfgPath))
	if err != nil {
		return err
	}
	defer cfgDir.Close()
	// 同一配置文件的多个接入操作串行执行，防止覆盖彼此新增的端点。
	lock, err := os.OpenFile(localsocket.Path(cfgDir, ".attach.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("另一个接入操作正在更新配置: %w", err)
	}
	actualConfig := localsocket.Path(cfgDir, filepath.Base(cfgPath))
	cfgInfo, err := os.Lstat(actualConfig)
	if err != nil {
		return err
	}
	if !cfgInfo.Mode().IsRegular() {
		return fmt.Errorf("配置必须为普通文件，不能是软链接")
	}
	c, err := config.Load(actualConfig)
	if err != nil {
		return err
	}
	if existing := c.Users[plan.ContainerName]; existing != "" && existing != o.User {
		return fmt.Errorf("已有容器用户映射为 %q，拒绝无意更换归属", existing)
	}
	replacement := "0"
	if o.ReplaceClient {
		replacement = "1"
	}
	if err = dockerScript(ctx, plan, linkScript, plan.ContainerDir, replacement, "0"); err != nil {
		return err
	}
	fmt.Fprintf(out, "容器: %s\n宿主机目录: %s\n容器内目录: %s\n用户: %s\n", plan.ContainerName, plan.HostDir, plan.ContainerDir, plan.User)
	if o.DryRun {
		fmt.Fprintln(out, "预检查完成，未安装客户端、未修改 YAML 或容器链接。")
		return nil
	}
	if !o.NoReload {
		available, e := command(ctx, "systemctl", "show", "--property=CanReload", "--value", "capybarad")
		if e != nil || strings.TrimSpace(string(available)) != "yes" {
			return fmt.Errorf("服务尚不支持 reload；先升级二进制和服务模板，或使用 --no-reload 后手动发送 SIGHUP")
		}
	}
	dir, err := localsocket.Directory(plan.HostDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	// 实际读取探针，确认容器没有被其他挂载遮蔽；不依赖镜像的 stat 实现。
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	marker := hex.EncodeToString(random[:])
	probeName := ".attach-probe-" + marker
	probe := localsocket.Path(dir, probeName)
	if err = os.WriteFile(probe, []byte(marker), 0644); err != nil {
		return err
	}
	defer os.Remove(probe)
	if err = dockerScript(ctx, plan, `set -eu; [ "$(cat "$1/$2")" = "$3" ]`, plan.ContainerDir, probeName, marker); err != nil {
		return fmt.Errorf("持久化目录在容器内不可见: %w", err)
	}
	if err = atomicCopy(dir, "capy", client, 0755); err != nil {
		return err
	}
	before, err := os.ReadFile(actualConfig)
	if err != nil {
		return err
	}
	updated, err := updateConfig(before, plan, c)
	if err != nil {
		return err
	}
	// 保留原配置备份，首次创建后不覆盖，便于管理员恢复原始接入前配置。
	backup := localsocket.Path(cfgDir, filepath.Base(cfgPath)+".before-attach")
	if f, e := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600); e == nil {
		_, err = f.Write(before)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !os.IsExist(e) {
		return e
	}
	if err = atomicCopy(cfgDir, filepath.Base(cfgPath), bytes.NewReader(updated), cfgInfo.Mode().Perm()); err != nil {
		return err
	}
	if err = dockerScript(ctx, plan, linkScript, plan.ContainerDir, replacement, "1"); err != nil {
		return fmt.Errorf("已写配置，但容器链接安装失败，可修复冲突后重试: %w", err)
	}
	if o.NoReload {
		fmt.Fprintln(out, "接入文件已安装；请对正在运行的新版 capybarad 发送 SIGHUP，或执行 systemctl reload capybarad。")
		return nil
	}
	if _, err = command(ctx, "systemctl", "reload", "capybarad"); err != nil {
		return fmt.Errorf("文件已安装，热加载失败，请检查服务日志后重试: %w", err)
	}
	// 等待真实端点出现，再从容器内验证身份和 CLI。失败时不假报已完成。
	deadline := time.Now().Add(10 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		conn, e := net.DialTimeout("unix", plan.Socket, 200*time.Millisecond)
		if e == nil {
			conn.Close()
			_, last = command(ctx, "docker", "exec", "--user", "0", plan.ContainerID, "/usr/local/bin/capy", "ls")
			if last == nil {
				fmt.Fprintln(out, "接入完成；容器未停止或重建，可在容器内执行 capy nodes。")
				return nil
			}
		} else {
			last = e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("接入验证未通过，配置已保留，请查看 journalctl -u capybarad: %w", last)
}

func atomicCopy(dir *os.File, name string, source io.Reader, mode os.FileMode) error {
	temp, err := os.CreateTemp(localsocket.Path(dir, ""), ".capy-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = io.Copy(temp, source); err == nil {
		err = temp.Chmod(mode)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(temp.Name(), localsocket.Path(dir, name)); err != nil {
		return err
	}
	return dir.Sync()
}

// updateConfig 修改 YAML 节点而不是重写整个结构，保留原有字段与注释。
func updateConfig(before []byte, p Plan, c config.Config) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(before, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("配置必须是 YAML 映射")
	}
	root := doc.Content[0]
	sockets := node(root, "additional_sockets", yaml.SequenceNode)
	if sockets.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("additional_sockets 必须是直接 YAML 序列，不支持别名")
	}
	exists := false
	for _, path := range c.SocketPaths() {
		if path == p.Socket {
			exists = true
		}
	}
	if !exists {
		sockets.Content = append(sockets.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: p.Socket})
	}
	users := node(root, "users", yaml.MappingNode)
	if users.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("users 必须是直接 YAML 映射，不支持别名")
	}
	found := false
	for i := 0; i < len(users.Content); i += 2 {
		if users.Content[i].Value == p.ContainerName {
			users.Content[i+1].Value = p.User
			found = true
		}
	}
	if !found {
		users.Content = append(users.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: p.ContainerName}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: p.User})
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func node(root *yaml.Node, key string, kind yaml.Kind) *yaml.Node {
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			n := root.Content[i+1]
			if n.Tag == "!!null" {
				n.Kind = kind
				n.Value = ""
				n.Tag = "!!map"
				if kind == yaml.SequenceNode {
					n.Tag = "!!seq"
				}
			}
			return n
		}
	}
	tag := "!!map"
	if kind == yaml.SequenceNode {
		tag = "!!seq"
	}
	n := &yaml.Node{Kind: kind, Tag: tag}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, n)
	return n
}
