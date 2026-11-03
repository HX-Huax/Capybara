// Package config 读取配置并拒绝未知字段，避免拼写错误静默生效。
package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Web 配置可选的本机管理员面板；令牌从环境变量读取，不写入配置或数据库。
type Web struct {
	Listen   string `yaml:"listen"`
	TokenEnv string `yaml:"token_env"`
}

// Config 保存 MVP 的部署参数。
type Config struct {
	Web                  Web               `yaml:"web"`
	Socket               string            `yaml:"socket"`
	SocketMode           string            `yaml:"socket_mode"`
	DataDir              string            `yaml:"data_dir"`
	ScheduleInterval     time.Duration     `yaml:"schedule_interval"`
	DefaultTimeLimit     time.Duration     `yaml:"default_time_limit"`
	CancelGrace          time.Duration     `yaml:"cancel_grace"`
	Users                map[string]string `yaml:"users"`
	ExternalThresholdMiB uint64            `yaml:"external_use_threshold_mib"`
}

// Load 在默认值上覆盖 YAML，所有时间必须为正数。
func Load(path string) (Config, error) {
	c := Config{Web: Web{TokenEnv: "CAPYBARA_WEB_TOKEN"}, Socket: "/run/capybara/capybara.sock", SocketMode: "0666", DataDir: "/var/lib/capybara", ScheduleInterval: 5 * time.Second, DefaultTimeLimit: 72 * time.Hour, CancelGrace: 30 * time.Second, ExternalThresholdMiB: 500}
	if path != "" {
		f, e := os.Open(path)
		if e != nil {
			return c, e
		}
		defer f.Close()
		d := yaml.NewDecoder(f)
		d.KnownFields(true)
		if e = d.Decode(&c); e != nil {
			return c, e
		}
	}
	if !strings.HasPrefix(c.Socket, "/") || !strings.HasPrefix(c.DataDir, "/") || c.ScheduleInterval <= 0 || c.DefaultTimeLimit <= 0 || c.CancelGrace <= 0 {
		return c, fmt.Errorf("路径必须为绝对路径，时间配置必须为正数")
	}
	if c.Web.Listen != "" {
		host, port, err := net.SplitHostPort(c.Web.Listen)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() || port == "" || c.Web.TokenEnv == "" {
			return c, fmt.Errorf("Web 监听必须使用 loopback IP:port，且需要 token_env")
		}
	}
	_, e := c.Mode()
	return c, e
}

// Mode 解析 Unix socket 的八进制权限。
func (c Config) Mode() (os.FileMode, error) {
	n, e := strconv.ParseUint(c.SocketMode, 8, 9)
	return os.FileMode(n), e
}
