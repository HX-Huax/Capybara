// Package dashboard 提供独立认证的管理员面板，不借用或伪造 Unix socket 身份。
package dashboard

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"capybara/internal/model"
	"capybara/internal/scheduler"
)

//go:embed assets/*
var assets embed.FS

// Server 的令牌只保留哈希；静态资源随二进制发布，无需 Node 或 CDN。
type Server struct {
	scheduler *scheduler.Scheduler
	tokenHash [32]byte
}

// New 使用至少 32 字节的令牌启用管理员访问。
func New(s *scheduler.Scheduler, token string) (*Server, error) {
	if len(token) < 32 || strings.TrimSpace(token) != token {
		return nil, errors.New("Web 管理令牌至少需要 32 字节，且不能包含首尾空白")
	}
	return &Server{scheduler: s, tokenHash: sha256.Sum256([]byte(token))}, nil
}

// Handler 静态登录页可公开读取，所有数据与控制接口都要求 Bearer 令牌。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "不支持的方法", 405)
			return
		}
		names := map[string]string{"/": "index.html", "/app.js": "app.js", "/style.css": "style.css", "/favicon.svg": "favicon.svg"}
		name, ok := names[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := assets.ReadFile("assets/" + name)
		if err != nil {
			http.Error(w, "资源不存在", 500)
			return
		}
		types := map[string]string{"index.html": "text/html; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "style.css": "text/css; charset=utf-8", "favicon.svg": "image/svg+xml"}
		w.Header().Set("Content-Type", types[name])
		_, _ = w.Write(b)
	})
	api := http.NewServeMux()
	api.HandleFunc("GET /api/overview", s.overview)
	api.HandleFunc("GET /api/containers", s.containers)
	api.HandleFunc("POST /api/containers/{id}/restart", s.restartContainer)
	api.HandleFunc("GET /api/jobs/{id}", s.job)
	api.HandleFunc("GET /api/jobs/{id}/logs", s.logs)
	api.HandleFunc("DELETE /api/jobs/{id}", s.cancel)
	api.HandleFunc("PATCH /api/jobs/{id}/priority", s.priority)
	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不使用 Cookie 或 URL 参数认证，跨站表单不能携带管理凭据。
		value := r.Header.Get("Authorization")
		if len(value) > 4096 || !strings.HasPrefix(value, "Bearer ") {
			http.Error(w, "请使用管理令牌登录", 401)
			return
		}
		got := sha256.Sum256([]byte(strings.TrimPrefix(value, "Bearer ")))
		if subtle.ConstantTimeCompare(got[:], s.tokenHash[:]) != 1 {
			http.Error(w, "管理令牌无效", 401)
			return
		}
		api.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		mux.ServeHTTP(w, r)
	})
}

func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.scheduler.Store.List()
	if err != nil {
		http.Error(w, "无法读取作业", 500)
		return
	}
	model.SortQueue(jobs)
	nodes, probeErr := s.scheduler.Nodes(r.Context())
	gpuError := ""
	if probeErr != nil {
		gpuError = probeErr.Error()
	}
	// 探测失败时仍返回队列，避免设备异常遮蔽作业和取消入口。
	respond(w, struct {
		Jobs      []model.Job `json:"jobs"`
		Nodes     any         `json:"nodes"`
		GPUError  string      `json:"gpu_error"`
		UpdatedAt time.Time   `json:"updated_at"`
	}{jobs, nodes, gpuError, time.Now().UTC()})
}
func (s *Server) load(w http.ResponseWriter, r *http.Request) (model.Job, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "无效作业 ID", 400)
		return model.Job{}, false
	}
	j, err := s.scheduler.Store.Get(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "作业不存在", 404)
		return j, false
	}
	if err != nil {
		http.Error(w, "无法读取作业", 500)
		return j, false
	}
	return j, true
}
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	if j, ok := s.load(w, r); ok {
		respond(w, j)
	}
}
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	j, ok := s.load(w, r)
	if !ok {
		return
	}
	if err := s.scheduler.Cancel(j.ID); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	respond(w, map[string]int64{"id": j.ID})
}
func (s *Server) priority(w http.ResponseWriter, r *http.Request) {
	j, ok := s.load(w, r)
	if !ok {
		return
	}
	var body struct {
		Priority *int `json:"priority"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Priority == nil || !model.ValidPriority(*body.Priority) {
		http.Error(w, "优先级必须为 -100 到 100 之间的整数", 400)
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		http.Error(w, "请求必须只有一个 JSON 对象", 400)
		return
	}
	updated, err := s.scheduler.SetPriority(j.ID, *body.Priority)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	respond(w, updated)
}
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	j, ok := s.load(w, r)
	if !ok {
		return
	}
	f, err := os.Open(s.scheduler.Runner.LogPath(j.ID))
	if os.IsNotExist(err) {
		respond(w, map[string]any{"text": "", "truncated": false})
		return
	}
	if err != nil {
		http.Error(w, "无法读取日志", 500)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "无法读取日志", 500)
		return
	}
	// 日志轮询只传输末尾 64 KiB，不会因训练日志不断增长而耗尽浏览器内存。
	const max = 64 << 10
	offset := info.Size() - max
	if offset < 0 {
		offset = 0
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		http.Error(w, "无法读取日志", 500)
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		http.Error(w, "无法读取日志", 500)
		return
	}
	respond(w, map[string]any{"text": string(b), "truncated": offset > 0})
}
