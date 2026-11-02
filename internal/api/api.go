// Package api 在 Unix socket 上提供带容器身份校验的 HTTP API。
package api

import (
	"capybara/internal/identity"
	"capybara/internal/model"
	"capybara/internal/scheduler"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type identityKey struct{}

// Server 为每个请求重新确认容器身份。
type Server struct {
	Scheduler *scheduler.Scheduler
	Users     map[string]string
}

// Handler 注册 MVP 路由。
func (s Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/jobs", s.submit)
	m.HandleFunc("GET /v1/jobs", s.list)
	m.HandleFunc("GET /v1/jobs/{id}", s.job)
	m.HandleFunc("DELETE /v1/jobs/{id}", s.job)
	m.HandleFunc("GET /v1/jobs/{id}/logs", s.job)
	m.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		d, e := s.Scheduler.Probe(r.Context())
		if e != nil {
			http.Error(w, e.Error(), 503)
			return
		}
		jobs, e := s.Scheduler.Store.List()
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		for i := range d {
			for _, j := range jobs {
				if j.State == "running" {
					for _, uuid := range j.GPUs {
						if uuid == d[i].UUID {
							d[i].JobID = j.ID
							d[i].External = false
						}
					}
				}
			}
		}
		write(w, d)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, e := identity.Resolve(r.Context(), s.Users)
		if e != nil {
			http.Error(w, "身份校验失败: "+e.Error(), 403)
			return
		}
		m.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, who)))
	})
}
func write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (s Server) submit(w http.ResponseWriter, r *http.Request) {
	who := r.Context().Value(identityKey{}).(identity.Identity)
	var e error
	var req model.Submit
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if e = d.Decode(&req); e != nil {
		http.Error(w, e.Error(), 400)
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "请求必须只有一个 JSON 对象", 400)
		return
	}
	limit := s.Scheduler.Limit
	if req.TimeLimit != "" {
		limit, e = time.ParseDuration(req.TimeLimit)
	}
	if e != nil || limit <= 0 || req.NGPUs < 0 || len(req.Command) == 0 || req.Command[0] == "" || !filepath.IsAbs(req.Cwd) {
		http.Error(w, "命令、工作目录、GPU 数量或时间限制无效", 400)
		return
	}
	for _, a := range req.Command {
		if strings.ContainsRune(a, 0) {
			http.Error(w, "参数包含 NUL", 400)
			return
		}
	}
	for k, v := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			http.Error(w, "环境变量无效", 400)
			return
		}
	}
	// 请求 UID 是容器命名空间内的身份，与宿主机 UID 可能不同；禁止容器内提权到其他 UID。
	if req.UID != who.UID {
		http.Error(w, "当前 MVP 不支持 userns UID 映射或指定其他用户", 403)
		return
	}
	j := model.Job{User: who.User, Container: who.Container, Command: req.Command, Cwd: req.Cwd, Env: req.Env, UID: req.UID, NGPUs: req.NGPUs, TimeLimit: limit}
	if e = s.Scheduler.Submit(&j); e != nil {
		http.Error(w, e.Error(), 500)
		return
	}
	w.WriteHeader(201)
	write(w, j)
}
func (s Server) list(w http.ResponseWriter, r *http.Request) {
	who := r.Context().Value(identityKey{}).(identity.Identity)
	jobs, e := s.Scheduler.Store.List()
	if e != nil {
		http.Error(w, e.Error(), 500)
		return
	}
	out := []model.Job{}
	for _, j := range jobs {
		if j.User != who.User {
			continue
		}
		if state := r.URL.Query().Get("state"); state != "" && j.State != state {
			continue
		}
		out = append(out, j)
	}
	write(w, out)
}
func (s Server) job(w http.ResponseWriter, r *http.Request) {
	id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if e != nil || id <= 0 {
		http.Error(w, "无效作业 ID", 400)
		return
	}
	j, e := s.Scheduler.Store.Get(id)
	if e != nil {
		http.Error(w, "作业不存在", 404)
		return
	}
	who := r.Context().Value(identityKey{}).(identity.Identity)
	if who.User != j.User {
		http.Error(w, "无权访问作业", 403)
		return
	}
	if r.Method == "DELETE" {
		if e = s.Scheduler.Cancel(id); e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		write(w, map[string]int64{"id": id})
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/logs") {
		write(w, j)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var offset int64
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		f, e := os.Open(s.Scheduler.Runner.LogPath(id))
		if e == nil {
			_, e = f.Seek(offset, io.SeekStart)
			if e == nil {
				var n int64
				n, e = io.Copy(w, f)
				offset += n
			}
			f.Close()
			if e != nil {
				return
			}
		} else if !os.IsNotExist(e) {
			http.Error(w, fmt.Sprint(e), 500)
			return
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		j, e = s.Scheduler.Store.Get(id)
		if e != nil || r.URL.Query().Get("follow") != "1" {
			return
		}
		if model.Terminal(j.State) {
			// 状态读取前可能刚写入最后一段日志，终态后再读取一次尾部。
			if f, err := os.Open(s.Scheduler.Runner.LogPath(id)); err == nil {
				defer f.Close()
				if _, err = f.Seek(offset, io.SeekStart); err == nil {
					_, _ = io.Copy(w, f)
				}
			}
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}
