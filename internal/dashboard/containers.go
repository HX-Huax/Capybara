package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var containerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

// 只查询展示所需字段，避免把 inspect 中的环境变量和挂载信息返回浏览器。
type containerView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Image       string `json:"image"`
	State       string `json:"state"`
	Status      string `json:"status"`
	Maintenance string `json:"maintenance"`
	RunningJobs int    `json:"running_jobs"`
}

func listContainers(ctx context.Context) ([]containerView, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--no-trunc", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, fmt.Errorf("查询 Docker 容器失败: %w", err)
	}
	out := []containerView{}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	for {
		var row struct{ ID, Names, Image, State, Status string }
		err = decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !containerID.MatchString(row.ID) {
			return nil, fmt.Errorf("Docker 返回无效容器 ID")
		}
		out = append(out, containerView{ID: row.ID, Name: row.Names, Image: row.Image, State: row.State, Status: row.Status})
	}
	return out, nil
}
func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	rows, err := listContainers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	jobs, err := s.scheduler.Store.List()
	if err != nil {
		http.Error(w, "无法读取作业", 500)
		return
	}
	maintenance := s.scheduler.RestartStates()
	for i := range rows {
		rows[i].Maintenance = maintenance[rows[i].ID]
		for _, j := range jobs {
			if j.Container == rows[i].ID && j.State == "running" {
				rows[i].RunningJobs++
			}
		}
	}
	respond(w, rows)
}
func (s *Server) restartContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !containerID.MatchString(id) {
		http.Error(w, "需要完整的容器 ID", 400)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || (body.Mode != "graceful" && body.Mode != "force") {
		http.Error(w, "mode 必须为 graceful 或 force", 400)
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		http.Error(w, "请求必须只有一个 JSON 对象", 400)
		return
	}
	rows, err := listContainers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	found := false
	for _, row := range rows {
		if row.ID == id {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "容器不存在", 404)
		return
	}
	finish, err := s.scheduler.BeginRestart(id)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	success := false
	defer func() { finish(success) }()
	// 浏览器关闭不应中断已经发出的重启；用独立超时等待 Docker 返回。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	timeout := 10
	if body.Mode == "force" {
		timeout = 0
	}
	output, err := exec.CommandContext(ctx, "docker", "restart", "--timeout", strconv.Itoa(timeout), id).CombinedOutput()
	if err != nil {
		http.Error(w, fmt.Sprintf("重启未确认，该容器已暂停调度，请检查后重试: %v %s", err, strings.TrimSpace(string(output))), 502)
		return
	}
	success = true
	respond(w, map[string]string{"id": id, "mode": body.Mode, "status": "restarted"})
}
