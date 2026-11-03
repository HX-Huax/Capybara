// Package gpu 使用 nvidia-smi 提供 MVP 阶段的设备快照。
package gpu

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Device 使用 UUID 作为分配标识，Index 仅供展示。
type Device struct {
	Name        string  `json:"name"`
	TotalMiB    uint64  `json:"total_mib"`
	Utilization *uint64 `json:"utilization,omitempty"`
	UUID        string  `json:"uuid"`
	Index       int     `json:"index"`
	UsedMiB     uint64  `json:"used_mib"`
	External    bool    `json:"external"`
	JobID       int64   `json:"job_id,omitempty"`
}

// Probe 在命令失败或输出异常时返回错误，调度器必须停止新的 GPU 分配。
func Probe(ctx context.Context, threshold uint64) ([]Device, error) {
	b, e := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=uuid,index,memory.used,memory.total,utilization.gpu,name", "--format=csv,noheader,nounits").Output()
	if e != nil {
		return nil, e
	}
	rows, e := csv.NewReader(strings.NewReader(string(b))).ReadAll()
	if e != nil {
		return nil, e
	}
	out := []Device{}
	for _, r := range rows {
		if len(r) != 6 {
			return nil, fmt.Errorf("无效 GPU 输出")
		}
		idx, e := strconv.Atoi(strings.TrimSpace(r[1]))
		if e != nil {
			return nil, e
		}
		mem, e := strconv.ParseUint(strings.TrimSpace(r[2]), 10, 64)
		if e != nil {
			return nil, e
		}
		total, e := strconv.ParseUint(strings.TrimSpace(r[3]), 10, 64)
		if e != nil {
			return nil, e
		}
		// 某些设备不提供利用率，保留 nil 供面板显示“暂无数据”，不影响调度。
		var utilization *uint64
		if n, err := strconv.ParseUint(strings.TrimSpace(r[4]), 10, 64); err == nil && n <= 100 {
			utilization = &n
		}
		out = append(out, Device{Name: strings.TrimSpace(r[5]), TotalMiB: total, Utilization: utilization, UUID: strings.TrimSpace(r[0]), Index: idx, UsedMiB: mem, External: mem > threshold})
	}
	return out, nil
}
