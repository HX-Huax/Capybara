package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWebConfiguration(t *testing.T) {
	c, err := Load("")
	if err != nil || c.Web.Listen != "" || c.Web.TokenEnv != "CAPYBARA_WEB_TOKEN" {
		t.Fatalf("默认配置改变: %+v %v", c.Web, err)
	}
	for _, test := range []struct {
		listen string
		valid  bool
	}{{"127.0.0.1:8080", true}, {"[::1]:8080", true}, {"0.0.0.0:8080", false}, {"192.168.1.2:8080", false}, {":8080", false}, {"invalid", false}} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err = os.WriteFile(path, []byte("web:\n  listen: \""+test.listen+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err = Load(path)
		if (err == nil) != test.valid {
			t.Fatalf("%s: %v", test.listen, err)
		}
	}
}

// 热加载只能改变监听集合、权限及用户映射；拒绝其他配置的静默部分生效。
func TestSocketPathsAndReload(t *testing.T) {
	base, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	next := base
	next.AdditionalSockets = []string{"/home/alice/.capybara/capybara.sock"}
	next.Users = map[string]string{"alice": "alice"}
	if err = base.CheckReload(next); err != nil {
		t.Fatal(err)
	}
	if len(next.SocketPaths()) != 2 || next.SocketPaths()[0] != base.Socket {
		t.Fatal(next.SocketPaths())
	}
	next.Web.Listen = "127.0.0.1:8080"
	if err = base.CheckReload(next); err == nil {
		t.Fatal("不应允许热加载 Web 配置")
	}
	for _, extra := range []string{base.Socket, "relative.sock", "/a/../b.sock"} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err = os.WriteFile(p, []byte("additional_sockets:\n  - "+extra+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = Load(p); err == nil {
			t.Fatalf("接受无效端点 %s", extra)
		}
	}
}
