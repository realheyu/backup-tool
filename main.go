package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Set by the Makefile when building a release.
var version = "dev"

func main() { os.Exit(cli()) }
func cli() int {
	flags := flag.NewFlagSet("backup-tool", flag.ContinueOnError)
	path := flags.String("config", "backup-tool.toml", "TOML 配置文件")
	ask := flags.Bool("ask-password", false, "运行时输入 MySQL 密码（不回显）")
	check := flags.Bool("check", false, "仅检查配置；配置错误仍尝试通知")
	ver := flags.Bool("version", false, "显示版本")
	parseErr := flags.Parse(os.Args[1:])
	if errors.Is(parseErr, flag.ErrHelp) {
		return 0
	}
	if parseErr == nil && *ver {
		fmt.Println("backup-tool", version)
		return 0
	}
	if parseErr == nil && flags.NArg() != 0 {
		parseErr = fmt.Errorf("不支持子命令；用法: backup-tool --config backup-tool.toml")
	}
	c, configErr := loadConfig(*path)
	resolveDingTalk(&c)
	err := errors.Join(parseErr, configErr)
	if *check && err == nil {
		fmt.Println("配置正确（未检查数据库连接、同步目标或客户端权限）")
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err == nil {
		var p *string
		p, err = passwordFor(ctx, c, *ask)
		if err == nil {
			err = runBackup(ctx, c, p)
		}
		if p != nil && err != nil && *p != "" {
			err = errors.New(strings.ReplaceAll(err.Error(), *p, "[已隐藏密码]"))
		}
	}
	if err != nil {
		reportFailure(c, err)
	}
	// Invalid configuration must never create state in an unvalidated path.
	if parseErr == nil && configErr == nil && !*check {
		if e := recordRunAndNotifyWeekly(c, err, time.Now()); e != nil {
			reportFailure(c, e)
			err = errors.Join(err, e)
		}
	}
	if parseErr != nil {
		return 2
	}
	if err != nil {
		return 1
	}
	fmt.Println("备份完成")
	return 0
}

func reportFailure(c Config, err error) {
	msg := redact(c, err.Error())
	fmt.Fprintln(os.Stderr, "任务失败:", msg)
	// Use an independent short context so an interrupted/expired backup can
	// still send its failure notification.
	nctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := notifyFailure(nctx, c, msg); e != nil {
		fmt.Fprintln(os.Stderr, "钉钉通知失败:", redact(c, e.Error()))
	}
}
func redact(c Config, s string) string {
	for _, v := range []string{c.DingTalk.Webhook, c.DingTalk.Secret,
		c.Sync.OSSAccessKeyID, c.Sync.OSSAccessKeySecret,
		os.Getenv("OSS_ACCESS_KEY_ID"), os.Getenv("OSS_ACCESS_KEY_SECRET")} {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[已隐藏]")
		}
	}
	if c.MySQL.Password != nil && *c.MySQL.Password != "" {
		s = strings.ReplaceAll(s, *c.MySQL.Password, "[已隐藏密码]")
	}
	if c.MySQL.PasswordEnv != "" {
		if p := os.Getenv(c.MySQL.PasswordEnv); p != "" {
			s = strings.ReplaceAll(s, p, "[已隐藏密码]")
		}
	}
	return s
}
