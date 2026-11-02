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
	UUID     string `json:"uuid"`
	Index    int    `json:"index"`
	UsedMiB  uint64 `json:"used_mib"`
	External bool   `json:"external"`
	JobID    int64  `json:"job_id,omitempty"`
}

// Probe 在命令失败或输出异常时返回错误，调度器必须停止新的 GPU 分配。
func Probe(ctx context.Context, threshold uint64) ([]Device, error) {
	b, e := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=uuid,index,memory.used", "--format=csv,noheader,nounits").Output()
	if e != nil {
		return nil, e
	}
	rows, e := csv.NewReader(strings.NewReader(string(b))).ReadAll()
	if e != nil {
		return nil, e
	}
	out := []Device{}
	for _, r := range rows {
		if len(r) != 3 {
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
		out = append(out, Device{UUID: strings.TrimSpace(r[0]), Index: idx, UsedMiB: mem, External: mem > threshold})
	}
	return out, nil
}
