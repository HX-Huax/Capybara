// Package model 定义客户端与服务端共享的数据类型。
package model

import "time"

// Job 是持久化的作业快照。Env 只能供执行器使用，不通过 API 返回。
type Job struct {
	ID        int64             `json:"id"`
	User      string            `json:"user"`
	Container string            `json:"container"`
	Command   []string          `json:"cmd"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"-"`
	UID       uint32            `json:"uid"`
	NGPUs     int               `json:"ngpus"`
	GPUs      []string          `json:"gpus"`
	State     string            `json:"state"`
	TimeLimit time.Duration     `json:"time_limit_ns"`
	ExitCode  *int              `json:"exit_code,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	SubmitAt  time.Time         `json:"submit_at"`
	StartAt   *time.Time        `json:"start_at,omitempty"`
	EndAt     *time.Time        `json:"end_at,omitempty"`
}

// Terminal 判断状态是否已结束。
func Terminal(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled", "timeout", "lost":
		return true
	}
	return false
}

// Submit 不接受用户名或容器，由服务端根据连接凭据补充身份。
type Submit struct {
	Command   []string          `json:"cmd"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	UID       uint32            `json:"uid"`
	NGPUs     int               `json:"ngpus"`
	TimeLimit string            `json:"time_limit"`
}
