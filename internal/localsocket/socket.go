// Package localsocket 在已有持久化目录内创建真实 Unix socket，保持 SO_PEERCRED。
package localsocket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Directory 逐级打开目录，拒绝软链接；返回的 FD 固定真实目录，防止用户重命名父目录后写到其他位置。
// 最后一层必须属于服务账号且不能由其他用户写入；中间目录可为用户已有的挂载源。
func Directory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("目录必须为规范绝对路径: %s", path)
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		created := false
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if e == syscall.ENOENT {
			mkdirErr := syscall.Mkdirat(fd, part, 0755)
			if mkdirErr != nil && mkdirErr != syscall.EEXIST {
				syscall.Close(fd)
				return nil, mkdirErr
			}
			created = mkdirErr == nil
			next, e = syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		}
		if e == nil && created {
			var stat syscall.Stat_t
			e = syscall.Fstat(next, &stat)
			if e == nil && stat.Uid != uint32(os.Geteuid()) {
				e = fmt.Errorf("目录在创建后被其他用户替换")
			}
			if e == nil {
				e = syscall.Fchmod(next, 0755)
			}
			if e != nil {
				syscall.Close(next)
			}
		}
		syscall.Close(fd)
		if e != nil {
			return nil, fmt.Errorf("打开目录 %s: %w", path, e)
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, fmt.Errorf("接入目录必须属于服务账号且不能允许组/其他用户写入: %s", path)
	}
	return f, nil
}

// Path 返回固定目录 FD 下的路径；不能在关闭 dir 后继续使用。
func Path(dir *os.File, name string) string {
	return fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), name)
}

type endpoint struct {
	listener *net.UnixListener
	dir      *os.File
	path     string
	info     os.FileInfo
}

func listen(path string, mode os.FileMode) (*endpoint, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 107 {
		return nil, fmt.Errorf("socket 路径必须为不超过 107 字节的规范绝对路径: %s", path)
	}
	dir, err := Directory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	actual := Path(dir, filepath.Base(path))
	success := false
	defer func() {
		if !success {
			dir.Close()
		}
	}()
	if info, e := os.Lstat(actual); e == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("socket 路径已有其他文件，拒绝覆盖: %s", path)
		}
		conn, e := net.DialTimeout("unix", actual, time.Second)
		if e == nil {
			conn.Close()
			return nil, fmt.Errorf("socket 正在使用: %s", path)
		}
		if !os.IsNotExist(e) && !isRefused(e) {
			return nil, e
		}
		if e = os.Remove(actual); e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: actual, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// Go 默认关闭时 unlink 路径；这里由目录 FD 和 inode 校验负责清理，避免 FD 复用误删文件。
	l.SetUnlinkOnClose(false)
	info, err := os.Lstat(actual)
	if err != nil {
		l.Close()
		return nil, err
	}
	ep := &endpoint{listener: l, dir: dir, path: actual, info: info}
	if err = os.Chmod(actual, mode); err != nil {
		ep.close()
		return nil, err
	}
	success = true
	return ep, nil
}
func isRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
func (e *endpoint) close() {
	_ = e.listener.Close()
	if info, err := os.Lstat(e.path); err == nil && os.SameFile(info, e.info) {
		_ = os.Remove(e.path)
	}
	_ = e.dir.Close()
}

type handlerBox struct{ handler http.Handler }

// Manager 原子替换认证配置，并先建立所有新端点，再移除旧端点。
// 任一新端点失败时回滚新增监听，原有请求与运行作业不受影响。
type Manager struct {
	mu        sync.Mutex
	endpoints map[string]*endpoint
	handler   atomic.Pointer[handlerBox]
	server    *http.Server
	Errors    chan error
	closed    bool
}

// New 创建共享 HTTP 服务；每个 listener 直接取得客户端 Unix 连接凭据。
func New(connContext func(context.Context, net.Conn) context.Context) *Manager {
	m := &Manager{endpoints: map[string]*endpoint{}, Errors: make(chan error, 1)}
	m.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.handler.Load().handler.ServeHTTP(w, r) }), ConnContext: connContext, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	return m
}

// Apply 热加载 socket 集合、权限及认证 handler，不重启调度器。
func (m *Manager) Apply(paths []string, mode os.FileMode, handler http.Handler) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("监听服务已关闭")
	}
	if len(paths) == 0 || handler == nil {
		return fmt.Errorf("至少需要一个 socket 和有效 handler")
	}
	desired := map[string]bool{}
	added := map[string]*endpoint{}
	rollback := func() {
		for _, ep := range added {
			ep.close()
		}
	}
	for _, path := range paths {
		if desired[path] {
			rollback()
			return fmt.Errorf("重复 socket: %s", path)
		}
		desired[path] = true
		if _, exists := m.endpoints[path]; exists {
			continue
		}
		ep, err := listen(path, mode)
		if err != nil {
			rollback()
			return err
		}
		added[path] = ep
	}
	// 权限更新也先暂存旧值；失败时恢复已修改的端点。
	type permissionChange struct {
		endpoint *endpoint
		old      os.FileMode
	}
	var changed []permissionChange
	restore := func(cause error) error {
		rollback()
		for _, change := range changed {
			if err := os.Chmod(change.endpoint.path, change.old); err != nil {
				cause = errors.Join(cause, err)
			}
		}
		return cause
	}
	for path, ep := range m.endpoints {
		if !desired[path] {
			continue
		}
		info, err := os.Lstat(ep.path)
		if err != nil {
			return restore(err)
		}
		if !os.SameFile(info, ep.info) {
			return restore(fmt.Errorf("socket 文件已被替换: %s", path))
		}
		if err = os.Chmod(ep.path, mode); err != nil {
			return restore(err)
		}
		changed = append(changed, permissionChange{ep, info.Mode().Perm()})
	}
	m.handler.Store(&handlerBox{handler})
	for path, ep := range added {
		m.endpoints[path] = ep
		go func(e *endpoint) {
			if err := m.server.Serve(e.listener); err != nil && err != http.ErrServerClosed && !isClosed(err) {
				select {
				case m.Errors <- err:
				default:
				}
			}
		}(ep)
	}
	for path, ep := range m.endpoints {
		if !desired[path] {
			ep.close()
			delete(m.endpoints, path)
		}
	}
	return nil
}
func isClosed(err error) bool { return errors.Is(err, net.ErrClosed) }

// Shutdown 停止接受连接，并在宽限期内结束请求；不删除 socket 所在目录。
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	err := m.server.Shutdown(ctx)
	_ = m.server.Close()
	for path, ep := range m.endpoints {
		ep.close()
		delete(m.endpoints, path)
	}
	return err
}
