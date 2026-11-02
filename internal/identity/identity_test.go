package identity

import (
	"strings"
	"testing"
)

func TestContainerID(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, v := range []string{"1:devices:/docker/" + id, "0::/system.slice/docker-" + id + ".scope", "0::/docker/" + id + "/child"} {
		if got := ContainerID(v); got != id {
			t.Fatalf("解析 %q 得到 %q", v, got)
		}
	}
	for _, v := range []string{"0::/user.slice", "0::/docker/abc", "0::/docker/" + id + "a"} {
		if ContainerID(v) != "" {
			t.Fatalf("错误接受 %q", v)
		}
	}
}
