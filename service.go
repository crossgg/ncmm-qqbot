package main

import (
	"context"
	"fmt"
	"log"

	"github.com/kardianos/service"
)

type program struct {
	app        *App
	cancelFunc context.CancelFunc
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFunc = cancel
	go func() {
		if err := p.app.Run(ctx); err != nil {
			log.Printf("[QQBot-Service] 运行退出: %v", err)
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancelFunc != nil {
		p.cancelFunc()
	}
	p.app.Stop()
	return nil
}

func getService(app *App) (service.Service, error) {
	svcConfig := &service.Config{
		Name:        "ncmm-qqbot",
		DisplayName: "NCMM QQ 机器人后台服务",
		Description: "网易云音乐打卡助手 QQ 机器人长连接与通知中继后台守护服务",
		Arguments:   []string{"run"},
	}

	prg := &program{app: app}
	return service.New(prg, svcConfig)
}

func handleServiceAction(action string, app *App) error {
	svc, err := getService(app)
	if err != nil {
		return fmt.Errorf("创建服务对象失败: %w", err)
	}

	switch action {
	case "install":
		err = svc.Install()
		if err == nil {
			fmt.Println("✅ Windows/系统后台服务安装成功！")
			fmt.Println("👉 可执行: ncmm-qqbot service start 启动服务")
		}
		return err
	case "uninstall", "remove":
		err = svc.Uninstall()
		if err == nil {
			fmt.Println("✅ 后台服务已成功卸载！")
		}
		return err
	case "start":
		err = svc.Start()
		if err == nil {
			fmt.Println("🚀 后台服务已成功启动！")
		}
		return err
	case "stop":
		err = svc.Stop()
		if err == nil {
			fmt.Println("🛑 后台服务已停止！")
		}
		return err
	case "restart":
		_ = svc.Stop()
		err = svc.Start()
		if err == nil {
			fmt.Println("🔄 后台服务已重启！")
		}
		return err
	case "status":
		status, err := svc.Status()
		if err != nil {
			return err
		}
		statusText := "未知"
		switch status {
		case service.StatusRunning:
			statusText = "🟢 正在运行 (Running)"
		case service.StatusStopped:
			statusText = "⚪ 已停止 (Stopped)"
		}
		fmt.Printf("服务当前状态: %s\n", statusText)
		return nil
	default:
		return fmt.Errorf("未知服务指令 %q，支持的操作: install, uninstall, start, stop, restart, status", action)
	}
}
