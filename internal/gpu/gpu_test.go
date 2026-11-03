package gpu

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 利用率缺失不应阻止分配，但显存采样格式错误必须使探测失败。
func TestProbeMetrics(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	path := filepath.Join(dir, "nvidia-smi")
	write := func(output string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\ncat <<'DATA'\n"+output+"\nDATA\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("GPU-a, 0, 601, 8192, 42, NVIDIA GeForce RTX 3070\nGPU-b, 1, 0, 8192, [N/A], NVIDIA GPU")
	devices, err := Probe(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 || !devices[0].External || devices[0].TotalMiB != 8192 || devices[0].Utilization == nil || *devices[0].Utilization != 42 || devices[1].Utilization != nil {
		t.Fatalf("错误设备采样: %+v", devices)
	}
	write("GPU-a, 0, invalid, 8192, 0, NVIDIA GPU")
	if _, err = Probe(context.Background(), 500); err == nil {
		t.Fatal("无效显存被接受")
	}
}
