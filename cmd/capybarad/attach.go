package main

import (
	"capybara/internal/onboard"
	"context"
	"flag"
	"fmt"
	"os"
)

// runAttach 是宿主机管理员工具，不改变容器生命周期，也不走作业提交 API。
func runAttach(args []string) error {
	var options onboard.Options
	flags := flag.NewFlagSet("capybarad attach", flag.ContinueOnError)
	flags.StringVar(&options.Container, "container", "", "已有容器名称或 ID（必填）")
	flags.StringVar(&options.User, "user", "", "作业归属用户（必填）")
	flags.StringVar(&options.Mount, "mount", "/home", "容器内已有 bind mount 的目标目录")
	flags.StringVar(&options.Client, "client", "/usr/local/bin/capy", "新版客户端的宿主机路径")
	flags.StringVar(&options.Config, "config", "/etc/capybara/config.yaml", "守护进程 YAML 配置")
	flags.BoolVar(&options.ReplaceClient, "replace-client", false, "备份容器内已有的普通客户端文件，再替换为链接")
	flags.BoolVar(&options.NoReload, "no-reload", false, "只安装接入文件，稍后自行发送 SIGHUP")
	flags.BoolVar(&options.DryRun, "dry-run", false, "仅检查依赖与路径，输出接入计划")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if options.Container == "" || options.User == "" || flags.NArg() != 0 {
		return fmt.Errorf("用法: capybarad attach --container 容器名 --user 用户名 [--mount /home]")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("请在宿主机使用 sudo 运行 attach，以安装受保护的接入目录与容器内链接")
	}
	return onboard.Run(context.Background(), options, os.Stdout)
}
