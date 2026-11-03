package localsocket

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// socketTestDir 显式限制目录权限，避免 umask=0002/0000 时 t.TempDir 的
// 子目录成为组可写，导致正常功能测试意外触发生产环境的权限保护。
func socketTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

type peerKey struct{}

func peerContext(ctx context.Context, c net.Conn) context.Context {
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		return ctx
	}
	var cred *syscall.Ucred
	_ = raw.Control(func(fd uintptr) { cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED) })
	if cred == nil {
		return ctx
	}
	return context.WithValue(ctx, peerKey{}, cred.Pid)
}
func readSocket(path string) (string, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	r, err := client.Get("http://capy/")
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	return string(b), err
}
func shut(m *Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = m.Shutdown(ctx)
}

// 两个真实 socket 保留同一提交进程的内核 PID；热加载失败不能破坏原有监听。
func TestApplyCredentialsRollbackAndRemoval(t *testing.T) {
	dir := socketTestDir(t)
	a := filepath.Join(dir, "main.sock")
	b := filepath.Join(dir, "extra.sock")
	manager := New(peerContext)
	defer shut(manager)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "%v", r.Context().Value(peerKey{})) })
	if err := manager.Apply([]string{a}, 0600, handler); err != nil {
		t.Fatal(err)
	}
	if got, err := readSocket(a); err != nil || got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("凭据改变: %s %v", got, err)
	}
	if err := manager.Apply([]string{a, b}, 0600, handler); err != nil {
		t.Fatal(err)
	}
	if got, err := readSocket(b); err != nil || got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("额外端点凭据改变: %s %v", got, err)
	}
	if err := manager.Apply([]string{a, b, "/invalid/../path.sock"}, 0600, handler); err == nil {
		t.Fatal("无效路径应回滚")
	}
	if _, err := readSocket(a); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Apply([]string{b}, 0600, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "updated") })); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(a); !os.IsNotExist(err) {
		t.Fatal("移除端点未清理")
	}
	if got, err := readSocket(b); err != nil || got != "updated" {
		t.Fatalf("handler 未更新: %s %v", got, err)
	}
	shut(manager)
	now, err := os.Stat(dir)
	if err != nil || !os.SameFile(oldDir, now) {
		t.Fatal("关闭服务替换了持久化目录")
	}
	if _, err = os.Lstat(b); !os.IsNotExist(err) {
		t.Fatal("关闭未清理 socket")
	}
}

func TestPathConflictsAndSymlinks(t *testing.T) {
	dir := socketTestDir(t)
	real := filepath.Join(dir, "owned")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if f, err := Directory(link); err == nil {
		f.Close()
		t.Fatal("不应跟随目录软链接")
	}
	if err := os.Chmod(real, 0777); err != nil {
		t.Fatal(err)
	}
	if f, err := Directory(real); err == nil {
		f.Close()
		t.Fatal("不应接受其他用户可写目录")
	}
	if err := os.Chmod(real, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(real, "capy.sock")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if ep, err := listen(path, 0600); err == nil {
		ep.close()
		t.Fatal("覆盖了普通文件")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "keep" {
		t.Fatal("文件被修改")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	first, err := listen(path, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	if ep, err := listen(path, 0600); err == nil {
		ep.close()
		t.Fatal("覆盖了活跃 socket")
	}
}

// 使用固定目录 FD 清理，只能删除自身创建的 socket，不能误删原路径的新文件。
func TestPinnedDirectoryAndSafeCleanup(t *testing.T) {
	root := socketTestDir(t)
	dir := filepath.Join(root, "bundle")
	path := filepath.Join(dir, "capy.sock")
	ep, err := listen(path, 0600)
	if err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(root, "renamed")
	if err = os.Rename(dir, renamed); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	ep.close()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "replacement" {
		t.Fatal("错误清理替换文件")
	}
	if _, err = os.Lstat(filepath.Join(renamed, "capy.sock")); !os.IsNotExist(err) {
		t.Fatal("旧目录 socket 未清理")
	}
}

// systemd 的 UMask=0077 不能使新建接入目录对容器非 root 用户不可遍历。
func TestNewDirectoryWithRestrictiveUmask(t *testing.T) {
	root := socketTestDir(t)
	old := syscall.Umask(0077)
	defer syscall.Umask(old)
	dir, err := Directory(filepath.Join(root, "new", "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for _, path := range []string{filepath.Join(root, "new"), filepath.Join(root, "new", "bundle")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("目录权限不符合预期: %v %v", info, err)
		}
	}
}

// 即使某个端点已被外部替换，权限更新失败时也必须恢复其他端点的原权限。
func TestPermissionRollbackDoesNotTouchReplacement(t *testing.T) {
	root := socketTestDir(t)
	a := filepath.Join(root, "a.sock")
	b := filepath.Join(root, "b.sock")
	m := New(nil)
	defer shut(m)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	if err := m.Apply([]string{a, b}, 0600, handler); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "keep")
	if err := os.WriteFile(victim, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, b); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply([]string{a, b}, 0666, handler); err == nil {
		t.Fatal("不应更新已被替换的端点")
	}
	for _, path := range []string{a, victim} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("权限未恢复或修改了替换目标: %s %v", path, err)
		}
	}
}
