// capy 通过 Unix socket 提交和查看作业，不直接访问 Docker 或数据库。
package main

import (
	"bytes"
	"capybara/internal/model"
	"context"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if e := command().Execute(); e != nil {
		os.Exit(1)
	}
}
func command() *cobra.Command {
	var socket string
	root := &cobra.Command{Use: "capy", Short: "GPU 作业队列", SilenceUsage: true}
	root.PersistentFlags().StringVar(&socket, "socket", "/run/capybara/capybara.sock", "Unix socket 路径")
	request := func(ctx context.Context, method, path string, body any, out io.Writer) error {
		var b bytes.Buffer
		if body != nil {
			if e := json.NewEncoder(&b).Encode(body); e != nil {
				return e
			}
		}
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		req, e := http.NewRequestWithContext(ctx, method, "http://capy"+path, &b)
		if e != nil {
			return e
		}
		req.Header.Set("Content-Type", "application/json")
		resp, e := client.Do(req)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
		}
		_, e = io.Copy(out, resp.Body)
		return e
	}
	var ngpus int
	var limit string
	var envs []string
	submit := &cobra.Command{Use: "submit -g N [-t 24h] -- command [args...]", Short: "提交作业", Args: cobra.MinimumNArgs(1), RunE: func(c *cobra.Command, args []string) error {
		cwd, e := os.Getwd()
		if e != nil {
			return e
		}
		env := map[string]string{}
		for _, v := range envs {
			k, val, ok := strings.Cut(v, "=")
			if !ok {
				val, ok = os.LookupEnv(k)
				if !ok {
					return fmt.Errorf("环境变量 %s 未设置", k)
				}
			}
			env[k] = val
		}
		req := model.Submit{Command: args, Cwd: cwd, Env: env, UID: uint32(os.Getuid()), NGPUs: ngpus, TimeLimit: limit}
		return request(c.Context(), "POST", "/v1/jobs", req, c.OutOrStdout())
	}}
	submit.Flags().IntVarP(&ngpus, "gpus", "g", 1, "申请 GPU 数量")
	submit.Flags().StringVarP(&limit, "time-limit", "t", "", "运行时间限制")
	submit.Flags().StringArrayVar(&envs, "env", nil, "传递环境变量 KEY 或 KEY=VALUE（可重复）")
	root.AddCommand(submit)
	root.AddCommand(&cobra.Command{Use: "ls", Short: "列出自己的作业", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		return request(c.Context(), "GET", "/v1/jobs", nil, c.OutOrStdout())
	}})
	root.AddCommand(&cobra.Command{Use: "nodes", Short: "查看 GPU 状态", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		return request(c.Context(), "GET", "/v1/nodes", nil, c.OutOrStdout())
	}})
	for _, name := range []string{"info", "cancel", "logs"} {
		name := name
		var follow bool
		c := &cobra.Command{Use: name + " ID", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
			var id int64
			if _, e := fmt.Sscan(args[0], &id); e != nil || id <= 0 {
				return fmt.Errorf("无效作业 ID")
			}
			path := fmt.Sprintf("/v1/jobs/%d", id)
			method := "GET"
			if name == "cancel" {
				method = "DELETE"
			}
			if name == "logs" {
				path += "/logs"
				if follow {
					path += "?follow=1"
				}
			}
			return request(c.Context(), method, path, nil, c.OutOrStdout())
		}}
		if name == "logs" {
			c.Flags().BoolVarP(&follow, "follow", "f", false, "跟随日志")
		}
		root.AddCommand(c)
	}
	return root
}
