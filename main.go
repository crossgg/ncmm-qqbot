package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/crossgg/ncmm-qqbot/bot"
	"github.com/crossgg/ncmm-qqbot/handler"
	"github.com/crossgg/ncmm-qqbot/server"
)

type App struct {
	cm       *ConfigManager
	botCli   *bot.Client
	gw       *bot.GatewayManager
	handler  *handler.Handler
	srv      *server.Server
	gwCtx    context.Context
	gwCancel context.CancelFunc
	mu       sync.Mutex
}

func NewApp(configPath string) (*App, error) {
	cm := NewConfigManager(configPath)
	cfg, err := cm.Load()
	if err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}

	botCli := bot.NewClient(cfg.AppID, cfg.ClientSecret)

	app := &App{
		cm:     cm,
		botCli: botCli,
	}

	hAdapter := &handlerAdapter{app: app}
	h := handler.NewHandler(botCli, hAdapter)
	app.handler = h

	gw := bot.NewGatewayManager(botCli, h.HandleMessage)
	app.gw = gw

	sAdapter := &serverAdapter{app: app}
	srv := server.NewServer(sAdapter, botCli, app)
	app.srv = srv

	return app, nil
}

type handlerAdapter struct {
	app *App
}

func (ha *handlerAdapter) Get() handler.ConfigData {
	c := ha.app.cm.Get()
	return handler.ConfigData{
		AppID:        c.AppID,
		ClientSecret: c.ClientSecret,
		AdminOpenID:  c.AdminOpenID,
		Port:         c.Port,
		Host:         c.Host,
		NCMMHome:     c.NCMMHome,
		NCMMExe:      c.NCMMExe,
	}
}

func (ha *handlerAdapter) Save(d *handler.ConfigData) error {
	newCfg := &Config{
		AppID:        d.AppID,
		ClientSecret: d.ClientSecret,
		AdminOpenID:  d.AdminOpenID,
		Port:         d.Port,
		Host:         d.Host,
		NCMMHome:     d.NCMMHome,
		NCMMExe:      d.NCMMExe,
	}
	if err := ha.app.cm.Save(newCfg); err != nil {
		return err
	}
	ha.app.botCli.UpdateCredentials(newCfg.AppID, newCfg.ClientSecret)
	ha.app.Restart(context.Background())
	return nil
}

type serverAdapter struct {
	app *App
}

func (sa *serverAdapter) Get() server.ConfigDTO {
	c := sa.app.cm.Get()
	return server.ConfigDTO{
		AppID:        c.AppID,
		ClientSecret: c.ClientSecret,
		AdminOpenID:  c.AdminOpenID,
		Port:         c.Port,
		Host:         c.Host,
		NCMMHome:     c.NCMMHome,
		NCMMExe:      c.NCMMExe,
	}
}

func (sa *serverAdapter) Save(d *server.ConfigDTO) error {
	newCfg := &Config{
		AppID:        d.AppID,
		ClientSecret: d.ClientSecret,
		AdminOpenID:  d.AdminOpenID,
		Port:         d.Port,
		Host:         d.Host,
		NCMMHome:     d.NCMMHome,
		NCMMExe:      d.NCMMExe,
	}
	if err := sa.app.cm.Save(newCfg); err != nil {
		return err
	}
	sa.app.botCli.UpdateCredentials(newCfg.AppID, newCfg.ClientSecret)
	return nil
}

// 适配器：给 server.GatewayStatusGetter
func (a *App) GetStatus() (bot.GatewayStatus, string) {
	return a.gw.GetStatus()
}

func (a *App) Restart(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.gw.Stop()
	cfg := a.cm.Get()
	if cfg.AppID != "" && cfg.ClientSecret != "" {
		log.Println("[QQBot] 重新启动 QQ 网关连接...")
		a.gw.Start(a.gwCtx)
	}
}

func (a *App) Run(ctx context.Context) error {
	a.mu.Lock()
	a.gwCtx = ctx
	a.mu.Unlock()

	cfg := a.cm.Get()
	if cfg.AppID != "" && cfg.ClientSecret != "" {
		log.Println("[QQBot] 启动 QQ 机器人 WebSocket 长连接网关...")
		a.gw.Start(ctx)
	} else {
		log.Printf("[QQBot] ⚠️ 尚未配置 QQ 机器人凭证 (AppID / ClientSecret)，请访问 Web 面板进行在线配置: http://%s:%d", cfg.Host, cfg.Port)
	}

	// 启动 Web 管理面板与本地 Notify 接收器
	return a.srv.Start(ctx)
}

func (a *App) Stop() {
	a.gw.Stop()
}

func main() {
	exeDir, _ := os.Executable()
	baseDir := filepath.Dir(exeDir)
	cfgPath := filepath.Join(baseDir, "config.yaml")

	app, err := NewApp(cfgPath)
	if err != nil {
		log.Fatalf("初始化应用失败: %v", err)
	}

	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "service":
			if len(args) < 2 {
				fmt.Println("用法: ncmm-qqbot service <install|uninstall|start|stop|restart|status>")
				os.Exit(1)
			}
			if err := handleServiceAction(args[1], app); err != nil {
				fmt.Printf("执行服务操作失败: %v\n", err)
				os.Exit(1)
			}
			return
		case "run":
			// 前台直接运行
		case "-h", "--help", "help":
			printUsage()
			return
		}
	}

	// 默认直接前台运行
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("[QQBot] 收到退出信号，正在关闭...")
		cancel()
		app.Stop()
	}()

	cfg := app.cm.Get()
	fmt.Println("==================================================")
	fmt.Println("🤖 NCMM QQ 机器人插件已启动")
	fmt.Printf("🌐 在线配置管理面板: http://%s:%d\n", cfg.Host, cfg.Port)
	fmt.Printf("📢 本地通知中继地址: http://127.0.0.1:%d/notify\n", cfg.Port)
	fmt.Println("💡 提示: 初次使用请访问上方 Web 地址在线填写 AppID/密钥")
	fmt.Println("==================================================")

	if err := app.Run(ctx); err != nil {
		log.Printf("程序运行退出: %v", err)
	}
}

func printUsage() {
	fmt.Println(`NCMM QQ 机器人伴生守护插件 (ncmm-qqbot)

使用方式:
  ncmm-qqbot                   前台运行守护进程并输出实时日志
  ncmm-qqbot run               同上
  ncmm-qqbot help              查看帮助信息

伴生模式:
  安装并启用插件后，当启动 'ncmm web' 时，系统会自动在后台拉起本插件；
  当 'ncmm web' 停止时，本插件也会自动优雅退出，无需注册任何系统开机自启服务！

内部通信:
  默认监听 127.0.0.1:5606，纯本地回环通信，在 Docker 和宿主机上均无需对外暴露或映射端口。`)
}
