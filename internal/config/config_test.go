package config

import (
	"fmt"
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

// 开放监听必须显式启用，且不能绕过地址与令牌环境变量校验。
func TestRemoteWebConfiguration(t *testing.T) {
	for _, tc := range []struct {
		addr  string
		allow bool
		valid bool
	}{
		{"0.0.0.0:8080", true, true}, {"192.168.3.69:8080", true, true},
		{"[::]:8080", true, true}, {"192.168.3.69:8080", false, false},
		{"127.0.0.1:8080", true, true}, {":8080", true, false}, {"invalid", true, false},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		body := fmt.Sprintf("web:\n  listen: %q\n  allow_remote: %t\n", tc.addr, tc.allow)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if (err == nil) != tc.valid {
			t.Fatalf("%s allow=%t: %v", tc.addr, tc.allow, err)
		}
		if err == nil && c.Web.TokenEnv != "CAPYBARA_WEB_TOKEN" {
			t.Fatal("令牌配置被改变")
		}
	}
	base, _ := Load("")
	next := base
	next.Web.AllowRemote = true
	if err := base.CheckReload(next); err == nil {
		t.Fatal("Web 访问范围变更应要求重启")
	}
}
