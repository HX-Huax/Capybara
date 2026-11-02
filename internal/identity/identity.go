// Package identity 将内核提供的连接身份映射到 Docker 容器。
package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Peer 来自 SO_PEERCRED，不能由 HTTP 请求体指定。
type Peer struct {
	PID int32
	UID uint32
}

// Identity 是校验后的作业归属。
type Identity struct {
	User, Container string
	UID             uint32
}
type key struct{}

var containerPattern = regexp.MustCompile(`(?:/docker/|docker-)([a-f0-9]{64})(?:\.scope)?(?:/|$)`)

// ContainerID 同时支持 cgroup v1 和 systemd cgroup v2。
func ContainerID(cgroup string) string {
	for _, line := range strings.Split(cgroup, "\n") {
		m := containerPattern.FindStringSubmatch(line)
		if len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// ConnContext 仅保存内核凭据，耗时的容器查询留给请求处理阶段。
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	u, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	raw, e := u.SyscallConn()
	if e != nil {
		return ctx
	}
	var cred *syscall.Ucred
	var inner error
	e = raw.Control(func(fd uintptr) {
		cred, inner = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if e != nil || inner != nil || cred == nil {
		return ctx
	}
	return context.WithValue(ctx, key{}, Peer{cred.Pid, cred.Uid})
}

// Resolve 拒绝无法识别的来源；Docker 名称只能通过配置映射为用户。
func Resolve(ctx context.Context, users map[string]string) (Identity, error) {
	p, ok := ctx.Value(key{}).(Peer)
	if !ok {
		return Identity{}, fmt.Errorf("缺少连接凭据")
	}
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", p.PID))
	if e != nil {
		return Identity{}, e
	}
	id := ContainerID(string(b))
	if id == "" {
		return Identity{}, fmt.Errorf("仅允许来自可识别 Docker 容器的连接")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, e = exec.CommandContext(ctx, "docker", "inspect", id).Output()
	if e != nil {
		return Identity{}, e
	}
	var rows []struct {
		ID     string `json:"Id"`
		Name   string
		State  struct{ Running bool }
		Config struct{ Labels map[string]string }
	}
	if e = json.Unmarshal(b, &rows); e != nil {
		return Identity{}, e
	}
	if len(rows) != 1 || rows[0].ID != id || !rows[0].State.Running {
		return Identity{}, fmt.Errorf("容器身份或状态无效")
	}
	user := rows[0].Config.Labels["capybara.user"]
	if user == "" {
		user = users[strings.TrimPrefix(rows[0].Name, "/")]
	}
	if user == "" {
		return Identity{}, fmt.Errorf("容器缺少用户标签或映射")
	}
	return Identity{user, id, p.UID}, nil
}
