package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crossgg/ncmm-qqbot/bot"
	"gopkg.in/yaml.v3"
)

type ConfigGetter interface {
	Get() ConfigData
	Save(*ConfigData) error
}

type ConfigData struct {
	AutoNotify   *bool
	AppID        string
	ClientSecret string
	AdminOpenID  string
	Port         int
	Host         string
	NCMMHome     string
	NCMMExe      string
}

type PhoneLoginSession struct {
	OpenID      string
	Cellphone   string
	CountryCode int64
	Target      string // 目标账号归属: "main" 或自定义如 "fan1.json"
	Captcha     string // 缓存验证码供 8860 新昵称注册使用
	Step        int    // 0: 等待输入验证码, 1: 等待输入新昵称(8860)
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type ExecRecord struct {
	Command   string
	Args      []string
	StartTime time.Time
	EndTime   time.Time
	Duration  time.Duration
	IsRunning bool
	ExitCode  int
	Output    string
	Err       error
	buf       *safeBuffer
}

type Handler struct {
	client     *bot.Client
	cfgGetter  ConfigGetter
	loginMu    sync.Mutex
	isLogining bool

	phoneMu       sync.RWMutex
	phoneSessions map[string]*PhoneLoginSession

	execMu     sync.Mutex
	activeExec *ExecRecord
	lastExec   *ExecRecord
}

func NewHandler(client *bot.Client, cfgGetter ConfigGetter) *Handler {
	return &Handler{
		client:        client,
		cfgGetter:     cfgGetter,
		phoneSessions: make(map[string]*PhoneLoginSession),
	}
}

func (h *Handler) replyText(ctx context.Context, openID, text, msgID string) {
	sendCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := h.client.SendC2CMessage(sendCtx, openID, text, msgID)
	if err != nil {
		log.Printf("[QQBot-Handler] 发送消息给 %s 失败: %v", openID, err)
	} else if resp != nil {
		log.Printf("[QQBot-Handler] 发送消息给 %s 成功 (msgID=%s, respID=%s)", openID, msgID, resp.ID)
	}
}

func (h *Handler) HandleMessage(ctx context.Context, msg *bot.C2CMessage) {
	senderOpenID := msg.Author.UserOpenID
	content := strings.TrimSpace(msg.Content)
	msgID := msg.ID

	log.Printf("[QQBot-Handler] 收到来自用户 %s 的私聊消息: %s (msgID: %s)", senderOpenID, content, msgID)

	cfg := h.cfgGetter.Get()
	adminID := strings.TrimSpace(cfg.AdminOpenID)

	// 1. 查询 OpenID 指令
	if strings.EqualFold(content, "/myid") || strings.EqualFold(content, "id") || strings.EqualFold(content, "/id") || strings.EqualFold(content, "whoami") {
		resp := fmt.Sprintf("你的 OpenID：\n%s\n\n", senderOpenID)
		if adminID == "" {
			resp += "⚠️ 当前系统尚未绑定管理员。\n发送 /bind 可将此账号设为管理员，或在 NCMM Web 插件面板中填入该 OpenID。"
		} else if adminID == senderOpenID {
			resp += "✅ 您是当前系统的认证管理员。\n发送 /help 查看所有可用功能指令。"
		} else {
			resp += "🔒 您不是当前系统的管理员。"
		}
		h.replyText(ctx, senderOpenID, resp, msgID)
		return
	}

	// 2. 绑定管理员指令
	if strings.EqualFold(content, "/bind") {
		if adminID != "" && adminID != senderOpenID {
			h.replyText(ctx, senderOpenID, "❌ 系统已绑定其他管理员，如需重置请在 NCMM Web 插件面板中修改。", msgID)
			return
		}
		newCfg := cfg
		newCfg.AdminOpenID = senderOpenID
		if err := h.cfgGetter.Save(&newCfg); err != nil {
			h.replyText(ctx, senderOpenID, "❌ 绑定管理员失败: "+err.Error(), msgID)
			return
		}
		h.replyText(ctx, senderOpenID, fmt.Sprintf("🎉 恭喜！已成功将您的账号绑定为管理员！\n你的 OpenID：\n%s\n\n发送 /help 查看所有可用功能指令。", senderOpenID), msgID)
		return
	}

	// 3. 若尚未绑定管理员，任何消息均提示 OpenID 与绑定方法（类似 qbot-push 体验）
	if adminID == "" {
		welcome := fmt.Sprintf("👋 收到您的消息！\n你的 OpenID：\n%s\n\n⚠️ 当前系统尚未绑定管理员：\n• 发送 /bind 立即绑定当前账号为管理员\n• 发送 /help 查看支持的指令与功能\n• 或在 NCMM Web 插件面板中直接保存管理员 OpenID", senderOpenID)
		h.replyText(ctx, senderOpenID, welcome, msgID)
		return
	}

	// 4. 管理员鉴权：若已设置管理员且非管理员发来消息，则拒绝执行并告知其 OpenID
	if adminID != senderOpenID {
		h.replyText(ctx, senderOpenID, fmt.Sprintf("你的 OpenID：\n%s\n\n🔒 权限不足：当前系统已绑定其他管理员，您无法使用控制功能。\n如需绑定此账号，请在 NCMM Web 插件面板中修改管理员 OpenID。", senderOpenID), msgID)
		return
	}

	// 5. 优先检测当前用户是否存在进行中的手机验证码登录会话
	if h.processPhoneSessionInput(ctx, senderOpenID, msgID, content) {
		return
	}

	// 6. 管理员指令路由
	// 6.1 优先检测是否附带了文件附件 (例如直接向机器人发送包含 Cookie 的 .txt/.json 文件)
	if len(msg.Attachments) > 0 {
		h.handleAttachment(ctx, senderOpenID, msgID, msg.Attachments)
		return
	}

	lower := strings.ToLower(content)
	switch {
	case lower == "/help" || lower == "help" || lower == "帮助" || lower == "/菜单":
		h.handleHelp(ctx, senderOpenID, msgID)

	case lower == "/status" || lower == "/info" || lower == "状态":
		h.handleStatus(ctx, senderOpenID, msgID)

	case strings.HasPrefix(lower, "/login phone") || strings.HasPrefix(lower, "/phone"):
		h.handlePhoneLogin(ctx, senderOpenID, msgID, content)

	case lower == "/cancel" || lower == "cancel" || lower == "取消":
		h.handleCancelSession(ctx, senderOpenID, msgID)

	case lower == "/login" || lower == "/qrcode" || lower == "扫码" || strings.HasPrefix(lower, "/login ") || strings.HasPrefix(lower, "/qrcode "):
		h.handleQrcodeLogin(ctx, senderOpenID, msgID, content)

	case lower == "/run" || strings.HasPrefix(lower, "/run ") || lower == "运行" || lower == "打卡":
		h.handleRunTask(ctx, senderOpenID, msgID, content)

	case strings.HasPrefix(lower, "/exec") || strings.HasPrefix(lower, "/cmd"):
		h.handleExecCommand(ctx, senderOpenID, msgID, content)

	case lower == "/log" || strings.HasPrefix(lower, "/log "):
		h.handleLogCommand(ctx, senderOpenID, msgID, content)

	case strings.HasPrefix(lower, "/cookie") || isCookieString(content):
		h.handleCookieUpdate(ctx, senderOpenID, msgID, content)

	default:
		reply := fmt.Sprintf("🤖 收到指令: %s\n你的 OpenID：\n%s\n\n发送 /help 查看可用功能，发送 /status 查看当前运行状态。", content, senderOpenID)
		h.replyText(ctx, senderOpenID, reply, msgID)
	}
}

func (h *Handler) handleHelp(ctx context.Context, openID, msgID string) {
	menu := `📖【NCMM QQ 机器人功能菜单】
------------------------
🔑 账号登录与 Cookie 更新：
• /phone <手机号>    ：手机验证码登录（支持多轮交互，安全便捷）
• /phone <手机号> main：登录并保存/覆盖为主账号（支持指定 fan1.json 等）
• /login 或 /qrcode  ：获取网易云登录授权链接（手机点击一键授权最便捷）
• 发送文件          ：直接发送包含 Cookie 的【.txt/.json】文件，秒级解析并绑定
• 发送文本          ：/cookie MUSIC_U=xxxxxx 或完整 Cookie
• 指定小号更新      ：/cookie <账号别名> MUSIC_U=xxxxxx

⚡ 控制与查询：
• /exec <命令> [参数]：手动执行任意 ncmm 命令（如 /exec musician-sign）
• /log [行数]       ：查看正在运行或最近一次命令的执行日志（默认10行）
• /run             ：立即触发日常打卡任务
• /status          ：查看各账号状态与配置文件
• /myid            ：查看我的 OpenID 标识
• /help            ：查看本帮助菜单

🌐 管理面板：访问本插件 Web 页面在线维护配置。`
	h.replyText(ctx, openID, menu, msgID)
}

func (h *Handler) handleStatus(ctx context.Context, openID, msgID string) {
	cfg := h.cfgGetter.Get()
	ncmmHome := cfg.NCMMHome
	if ncmmHome == "" {
		ncmmHome = "."
	}

	cfgPath := filepath.Join(ncmmHome, "config.yaml")
	if !fileExists(cfgPath) {
		cfgPath = filepath.Join(ncmmHome, "config", "config.yaml")
	}

	if !fileExists(cfgPath) {
		h.replyText(ctx, openID, fmt.Sprintf("⚠️ 未在 %s 找到 config.yaml 配置文件", ncmmHome), msgID)
		return
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		h.replyText(ctx, openID, "读取 config.yaml 失败: "+err.Error(), msgID)
		return
	}

	var hostCfg struct {
		Accounts struct {
			Main      string   `yaml:"main"`
			Secondary []string `yaml:"secondary"`
		} `yaml:"accounts"`
	}
	_ = yaml.Unmarshal(data, &hostCfg)

	var sb strings.Builder
	sb.WriteString("📊【NCMM 账号配置状态】\n------------------------\n")

	mainFile := hostCfg.Accounts.Main
	if mainFile == "" {
		mainFile = "未配置"
	}
	mainStatus := "❌ 文件不存在"
	resolvedMain := resolveAccountPath(ncmmHome, mainFile)
	if fileExists(resolvedMain) {
		mainStatus = "✅ 已存在"
	}
	sb.WriteString(fmt.Sprintf("👑 主账号: %s\n   状态: %s\n", mainFile, mainStatus))

	secCount := len(hostCfg.Accounts.Secondary)
	sb.WriteString(fmt.Sprintf("\n👥 辅助账号 (%d 个):\n", secCount))
	for i, sec := range hostCfg.Accounts.Secondary {
		secStatus := "❌ 文件不存在"
		resolvedSec := resolveAccountPath(ncmmHome, sec)
		if fileExists(resolvedSec) {
			secStatus = "✅ 正常"
		}
		sb.WriteString(fmt.Sprintf(" [%d] %s (%s)\n", i+1, sec, secStatus))
	}

	h.replyText(ctx, openID, sb.String(), msgID)
}

func (h *Handler) handleRunTask(ctx context.Context, openID, msgID, content string) {
	args := []string{"task"}
	parts := strings.Fields(content)
	if len(parts) > 1 {
		args = append(args, parts[1:]...)
	}
	h.executeCommandAsync(ctx, openID, msgID, args)
}

func (h *Handler) handleExecCommand(ctx context.Context, openID, msgID, content string) {
	var cmdStr string
	lower := strings.ToLower(content)
	if strings.HasPrefix(lower, "/exec") {
		cmdStr = strings.TrimSpace(content[5:])
	} else if strings.HasPrefix(lower, "/cmd") {
		cmdStr = strings.TrimSpace(content[4:])
	} else {
		cmdStr = strings.TrimSpace(content)
	}

	if cmdStr == "" {
		usage := "💡 /exec 指令用法说明：\n" +
			"------------------------\n" +
			"• 格式: /exec <子命令> [参数...]\n" +
			"• 示例: /exec musician-sign （签到网易云音乐人）\n" +
			"• 示例: /exec fans （执行乐迷团日常）\n" +
			"• 示例: /exec yunbei （执行云贝任务）\n" +
			"• 示例: /exec vip （执行黑胶VIP任务）\n" +
			"• 示例: /exec playids -f fan1.json （刷歌试听）\n" +
			"• 示例: /exec notify test （测试通知推送）\n\n" +
			"可随时发送 /log 查看执行日志与进度。"
		h.replyText(ctx, openID, usage, msgID)
		return
	}

	args, err := splitCommandArgs(cmdStr)
	if err != nil {
		h.replyText(ctx, openID, "❌ 命令解析失败: "+err.Error(), msgID)
		return
	}
	if len(args) == 0 {
		h.replyText(ctx, openID, "⚠️ 请输入要执行的子命令，例如: /exec musician-sign", msgID)
		return
	}

	// 安全与常驻命令拦截
	sub := strings.ToLower(args[0])
	if sub == "web" || sub == "daemon" {
		h.replyText(ctx, openID, fmt.Sprintf("⚠️ 无法在 QQ 聊天中运行常驻服务指令: ncmm %s", sub), msgID)
		return
	}

	// 拦截未带参数的交互式 login phone，引导使用专属的 /phone
	if sub == "login" && len(args) > 1 && strings.ToLower(args[1]) == "phone" {
		hasCaptchaOrSend := false
		for _, a := range args[2:] {
			if a == "--send-only" || strings.HasPrefix(a, "--captcha") {
				hasCaptchaOrSend = true
				break
			}
		}
		if !hasCaptchaOrSend {
			h.replyText(ctx, openID, "💡 手机验证码登录请直接发送 /phone 指令（支持 QQ 私聊多轮交互输入验证码）。", msgID)
			return
		}
	}

	h.executeCommandAsync(ctx, openID, msgID, args)
}

const errNoNcmmExe = "❌ 未检测到 ncmm 可执行程序：\n• 请在 WebUI 插件面板或配置文件中填写 ncmm_exe 绝对路径（如 D:\\ncmm\\ncmm.exe 或 /usr/local/bin/ncmm）；\n• 或确保将 ncmm 加入系统 PATH 环境变量后重启服务。"

func (h *Handler) executeCommandAsync(ctx context.Context, openID, msgID string, args []string) {
	cfg := h.cfgGetter.Get()
	ncmmExe := cfg.NCMMExe
	if ncmmExe == "" {
		h.replyText(ctx, openID, errNoNcmmExe, msgID)
		return
	}

	h.execMu.Lock()
	if h.activeExec != nil && h.activeExec.IsRunning {
		runningCmd := h.activeExec.Command
		elapsed := time.Since(h.activeExec.StartTime).Round(time.Second)
		h.execMu.Unlock()
		h.replyText(ctx, openID, fmt.Sprintf("⚠️ 当前已有命令正在后台执行中:\n> %s (已运行 %v)\n\n可发送 /log 查看当前执行进度，请等待完成后再发起新命令。", runningCmd, elapsed), msgID)
		return
	}

	cmdDisplay := "ncmm " + strings.Join(args, " ")
	record := &ExecRecord{
		Command:   cmdDisplay,
		Args:      args,
		StartTime: time.Now(),
		IsRunning: true,
		buf:       &safeBuffer{},
	}
	h.activeExec = record
	h.execMu.Unlock()

	h.replyText(ctx, openID, fmt.Sprintf("🚀 正在后台执行: %s\n任务已在后台运行中，可随时发送 /log 查看执行日志。", cmdDisplay), msgID)

	go func() {
		cmdCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		cmd := exec.CommandContext(cmdCtx, ncmmExe, args...)
		cmd.Dir = cfg.NCMMHome
		cmd.Stdout = record.buf
		cmd.Stderr = record.buf

		err := cmd.Run()

		h.execMu.Lock()
		record.IsRunning = false
		record.EndTime = time.Now()
		record.Duration = record.EndTime.Sub(record.StartTime)
		record.Output = record.buf.String()
		record.Err = err
		if cmd.ProcessState != nil {
			record.ExitCode = cmd.ProcessState.ExitCode()
		} else if err != nil {
			record.ExitCode = 1
		}
		h.lastExec = record
		h.activeExec = nil
		h.execMu.Unlock()
	}()
}

func (h *Handler) handleLogCommand(ctx context.Context, openID, msgID, content string) {
	lineCount := 10 // 默认打印 10 行 (符合用户偏好)
	parts := strings.Fields(content)
	if len(parts) > 1 {
		if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
			if n > 80 {
				lineCount = 80
			} else {
				lineCount = n
			}
		}
	}

	h.execMu.Lock()
	active := h.activeExec
	last := h.lastExec
	h.execMu.Unlock()

	if active != nil && active.IsRunning {
		rawOutput := active.buf.String()
		clean := stripAnsi(rawOutput)
		lastLines := getLastLines(clean, lineCount)
		if strings.TrimSpace(lastLines) == "" {
			lastLines = "(任务刚启动，暂未产生日志)"
		}
		elapsed := time.Since(active.StartTime).Round(time.Second)
		resp := fmt.Sprintf("⏳ 任务正在运行中 (已耗时 %v):\n%s\n\n--- 最新输出 (末尾 %d 行) ---\n%s",
			elapsed, active.Command, lineCount, lastLines)
		h.replyText(ctx, openID, resp, msgID)
		return
	}

	if last != nil {
		rawOutput := last.Output
		clean := stripAnsi(rawOutput)
		lastLines := getLastLines(clean, lineCount)
		if strings.TrimSpace(lastLines) == "" {
			lastLines = "(无输出日志)"
		}
		statusDesc := "✅ 成功"
		if last.Err != nil || last.ExitCode != 0 {
			statusDesc = fmt.Sprintf("❌ 失败 (code: %d)", last.ExitCode)
		}
		dur := last.Duration.Round(100 * time.Millisecond)
		endStr := last.EndTime.Format("15:04:05")
		resp := fmt.Sprintf("📄 最近执行记录: %s\n状态: %s (耗时: %v, 完成于 %s)\n\n--- 日志内容 (末尾 %d 行) ---\n%s",
			last.Command, statusDesc, dur, endStr, lineCount, lastLines)
		h.replyText(ctx, openID, resp, msgID)
		return
	}

	h.replyText(ctx, openID, "ℹ️ 暂无最近执行记录。可通过 /exec 或 /run 运行任务后再发送 /log 查看。", msgID)
}

type LoginJSONResult struct {
	UID         int64  `json:"uid"`
	Nickname    string `json:"nickname"`
	AvatarURL   string `json:"avatarUrl"`
	CookiePath  string `json:"cookiePath"`
	AccountPath string `json:"accountPath"`
	Main        bool   `json:"main"`
}

func (h *Handler) handleAttachment(ctx context.Context, openID, msgID string, atts []bot.MessageAttachment) {
	for _, att := range atts {
		if att.URL == "" {
			continue
		}
		log.Printf("[QQBot-Handler] 收到来自用户 %s 的附件: filename=%s, url=%s, size=%d", openID, att.Filename, att.URL, att.Size)

		if att.Size > 1024*1024 {
			h.replyText(ctx, openID, fmt.Sprintf("⚠️ 附件【%s】体积过大（超过 1MB），已跳过处理。", att.Filename), msgID)
			continue
		}

		h.replyText(ctx, openID, fmt.Sprintf("📥 收到附件【%s】，正在下载并识别网易云 Cookie 凭据...", att.Filename), msgID)

		fileBytes, err := downloadAttachment(ctx, att.URL)
		if err != nil {
			h.replyText(ctx, openID, fmt.Sprintf("❌ 下载附件【%s】失败: %v", att.Filename, err), msgID)
			continue
		}

		fileText := strings.TrimSpace(string(fileBytes))
		fileText = strings.TrimPrefix(fileText, "\xef\xbb\xbf") // 移除 UTF-8 BOM

		if !isCookieString(fileText) {
			h.replyText(ctx, openID, fmt.Sprintf("⚠️ 附件【%s】下载完成，但未检测到有效网易云 Cookie 凭据（请确认文件内容是否包含 MUSIC_U 等字段）。", att.Filename), msgID)
			continue
		}

		var alias string
		baseName := strings.TrimSuffix(att.Filename, filepath.Ext(att.Filename))
		if strings.EqualFold(baseName, "main") {
			alias = "main"
		} else if strings.HasPrefix(strings.ToLower(baseName), "fan_") {
			alias = baseName
		}

		h.processCookieString(ctx, openID, msgID, alias, fileText, fmt.Sprintf("文件【%s】", att.Filename))
		return
	}
}

func downloadAttachment(ctx context.Context, fileURL string) ([]byte, error) {
	dlCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, "GET", fileURL, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载文件返回 HTTP %d", resp.StatusCode)
	}

	lr := io.LimitReader(resp.Body, 512*1024)
	return io.ReadAll(lr)
}

func (h *Handler) handleCookieUpdate(ctx context.Context, openID, msgID, content string) {
	alias, cookieStr := parseCookieInput(content)
	if cookieStr == "" {
		msg := "💡 Cookie 绑定与更新说明：\n" +
			"------------------------\n" +
			"1️⃣ 发送文件（强烈推荐）：\n" +
			"• 直接向机器人发送包含 Cookie 的【.txt】或【.json】文本文件\n" +
			"• 彻底避免长文本密文被 QQ 平台安全策略拦截！\n\n" +
			"2️⃣ 授权登录（最便捷）：\n" +
			"• 发送 /login 即可获取网易云登录授权链接\n\n" +
			"3️⃣ 发送文本：\n" +
			"• 自动识别并保存: /cookie MUSIC_U=xxxxxx\n" +
			"• 指定绑定为主账号: /cookie main MUSIC_U=xxxxxx\n" +
			"• 指定绑定为副账号: /cookie 4265 MUSIC_U=xxxxxx"
		h.replyText(ctx, openID, msg, msgID)
		return
	}

	h.processCookieString(ctx, openID, msgID, alias, cookieStr, "文本指令")
}

func (h *Handler) processCookieString(ctx context.Context, openID, msgID, alias, cookieStr, sourceDesc string) {
	cfg := h.cfgGetter.Get()
	ncmmExe := cfg.NCMMExe
	if ncmmExe == "" {
		h.replyText(ctx, openID, errNoNcmmExe, msgID)
		return
	}

	// 1. 临时预校验：使用 --no-config-write 与 --json-result 探测该 Cookie 对应的 UID 与昵称
	h.replyText(ctx, openID, fmt.Sprintf("🔍 正在连接网易云验证来自%s的 Cookie 并自动识别账号身份...", sourceDesc), msgID)

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer probeCancel()

	probeCmd := exec.CommandContext(probeCtx, ncmmExe, "login", "cookie", "--no-config-write", "--json-result", cookieStr)
	probeCmd.Dir = cfg.NCMMHome

	var probeOut, probeErr bytes.Buffer
	probeCmd.Stdout = &probeOut
	probeCmd.Stderr = &probeErr

	err := probeCmd.Run()
	if err != nil {
		errInfo := probeErr.String()
		if errInfo == "" {
			errInfo = probeOut.String()
		}
		h.replyText(ctx, openID, fmt.Sprintf("❌ Cookie 校验失败: %v\n%s", err, errInfo), msgID)
		return
	}

	res, ok := parseLoginResult(probeOut.String())
	if !ok || res.UID == 0 {
		h.replyText(ctx, openID, "❌ 登录成功但无法解析账号信息: \n"+probeOut.String(), msgID)
		return
	}

	// 2. 匹配账号归属
	var isMain bool
	var targetFile string
	if alias != "" {
		if strings.EqualFold(alias, "main") || alias == "主账号" || alias == "主" {
			isMain = true
		} else {
			isMain = false
			if !strings.HasSuffix(strings.ToLower(alias), ".json") {
				targetFile = alias + ".json"
			} else {
				targetFile = alias
			}
		}
	} else {
		isMain, targetFile = h.detectAccountTarget(cfg.NCMMHome, res.UID)
	}

	// 3. 执行正式回写命令
	var finalArgs []string
	if isMain {
		finalArgs = []string{"login", "cookie", "--main", cookieStr}
	} else if targetFile != "" {
		finalArgs = []string{"login", "cookie", "-o", targetFile, cookieStr}
	} else {
		finalArgs = []string{"login", "cookie", cookieStr}
	}

	saveCtx, saveCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer saveCancel()

	saveCmd := exec.CommandContext(saveCtx, ncmmExe, finalArgs...)
	saveCmd.Dir = cfg.NCMMHome
	if err := saveCmd.Run(); err != nil {
		h.replyText(ctx, openID, "❌ 保存 Cookie 到系统失败: "+err.Error(), msgID)
		return
	}

	accountType := "【主账号】"
	if !isMain {
		accountType = "【辅助小号】"
	}

	reply := fmt.Sprintf("🎉 来自%s的 Cookie 自动识别并更新成功！\n------------------------\n身份类型: %s\n用户昵称: %s\n用户 UID: %d\n落盘位置: %s",
		sourceDesc, accountType, res.Nickname, res.UID, res.AccountPath)
	h.replyText(ctx, openID, reply, msgID)
}

func (h *Handler) detectAccountTarget(ncmmHome string, uid int64) (isMain bool, targetFile string) {
	cfgPath := filepath.Join(ncmmHome, "config.yaml")
	if !fileExists(cfgPath) {
		cfgPath = filepath.Join(ncmmHome, "config", "config.yaml")
	}
	if !fileExists(cfgPath) {
		return true, "" // 无配置时默认作为主账号
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return true, ""
	}

	var hostCfg struct {
		Accounts struct {
			Main      string   `yaml:"main"`
			Secondary []string `yaml:"secondary"`
		} `yaml:"accounts"`
	}
	_ = yaml.Unmarshal(data, &hostCfg)

	// 1. 比对主账号文件
	mainResolved := resolveAccountPath(ncmmHome, hostCfg.Accounts.Main)
	if fileContainsUID(mainResolved, uid) {
		return true, ""
	}

	// 2. 比对辅助账号文件
	for _, sec := range hostCfg.Accounts.Secondary {
		secResolved := resolveAccountPath(ncmmHome, sec)
		if fileContainsUID(secResolved, uid) {
			return false, sec
		}
	}

	// 3. 辅助账号文件名直接包含 UID (如 fan_123456.json)
	uidStr := fmt.Sprintf("%d", uid)
	for _, sec := range hostCfg.Accounts.Secondary {
		if strings.Contains(sec, uidStr) {
			return false, sec
		}
	}

	// 4. 若主账号尚不存在，则首个录入的作为主账号
	if !fileExists(mainResolved) {
		return true, ""
	}

	// 5. 否则作为全新辅助账号
	return false, ""
}

func fileContainsUID(path string, uid int64) bool {
	if !fileExists(path) {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// 在 cookie 文件中搜索 UID 或 P_INFO 中的 UID
	uidPattern := fmt.Sprintf("%d", uid)
	return strings.Contains(string(data), uidPattern)
}

func (h *Handler) handleQrcodeLogin(ctx context.Context, openID, msgID, content string) {
	h.loginMu.Lock()
	if h.isLogining {
		h.loginMu.Unlock()
		h.replyText(ctx, openID, "⚠️ 当前已有一个扫码登录会话正在进行中，请在手机上确认或稍候重试。", msgID)
		return
	}
	h.isLogining = true
	h.loginMu.Unlock()

	defer func() {
		h.loginMu.Lock()
		h.isLogining = false
		h.loginMu.Unlock()
	}()

	cfg := h.cfgGetter.Get()
	ncmmExe := cfg.NCMMExe
	if ncmmExe == "" {
		h.replyText(ctx, openID, errNoNcmmExe, msgID)
		return
	}

	h.replyText(ctx, openID, "📱 正在生成网易云音乐登录授权链接，请稍候...", msgID)

	qrCmdCtx, qrCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer qrCancel()

	// 启动 ncmm login qrcode
	cmd := exec.CommandContext(qrCmdCtx, ncmmExe, "login", "qrcode")
	cmd.Dir = cfg.NCMMHome

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		h.replyText(ctx, openID, "❌ 创建管道失败: "+err.Error(), msgID)
		return
	}
	var outputBuf bytes.Buffer
	cmd.Stderr = &outputBuf

	if err := cmd.Start(); err != nil {
		h.replyText(ctx, openID, "❌ 启动登录命令失败: "+err.Error(), msgID)
		return
	}

	codeKeyChan := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(io.TeeReader(stdoutPipe, &outputBuf))
		reKey := regexp.MustCompile(`codekey=([a-zA-Z0-9-]+)`)
		foundKey := false
		for scanner.Scan() {
			line := scanner.Text()
			if !foundKey && reKey.MatchString(line) {
				m := reKey.FindStringSubmatch(line)
				if len(m) > 1 {
					foundKey = true
					codeKeyChan <- m[1]
				}
			}
		}
		if !foundKey {
			close(codeKeyChan)
		}
	}()

	select {
	case codeKey, ok := <-codeKeyChan:
		if ok && codeKey != "" {
			hintMsg := fmt.Sprintf("👉 请使用手机点击以下授权链接完成登录：\nhttps://music.163.com/login?codekey=%s\n\n⏰ 有效时间 5 分钟，在手机网易云音乐确认授权后将自动更新并绑定凭据！", codeKey)
			h.replyText(ctx, openID, hintMsg, msgID)
		} else {
			h.replyText(ctx, openID, "⚠️ 未能从服务中获取到授权标识，请稍候重试。", msgID)
		}
	case <-time.After(10 * time.Second):
		h.replyText(ctx, openID, "⚠️ 获取授权链接超时，请稍候重试。", msgID)
	}

	// 等待授权完成
	waitErr := cmd.Wait()
	if waitErr != nil {
		h.replyText(context.Background(), openID, "⏳ 登录已超时或已取消。", "")
		return
	}

	out := outputBuf.String()
	nickname := "未知"
	if re := regexp.MustCompile(`nickname=([^\s\r\n]+)`); re.MatchString(out) {
		m := re.FindStringSubmatch(out)
		if len(m) > 1 {
			nickname = m[1]
		}
	}

	successMsg := fmt.Sprintf("🎉 登录成功！\n账号【%s】Cookie 已更新并自动回写到系统配置中！", nickname)
	h.replyText(context.Background(), openID, successMsg, "")
}

func isCookieString(s string) bool {
	upper := strings.ToUpper(s)
	return strings.Contains(upper, "MUSIC_U") ||
		strings.Contains(upper, "MUSIC_A_T") ||
		strings.Contains(upper, "_NTES_NUID") ||
		strings.Contains(upper, "__CSRF") ||
		strings.Contains(upper, "P_INFO=")
}

func parseCookieInput(content string) (alias string, cookieStr string) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(strings.ToLower(content), "/cookie") {
		rest := strings.TrimSpace(content[7:])
		parts := strings.Fields(rest)
		if len(parts) >= 2 && !strings.Contains(parts[0], "=") {
			alias = parts[0]
			cookieStr = strings.TrimSpace(rest[len(alias):])
			return alias, cookieStr
		}
		return "", rest
	}
	return "", content
}

func resolveAccountPath(home, path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if strings.HasPrefix(path, "${HOME}") {
		uHome, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(uHome, strings.TrimPrefix(path, "${HOME}"))
		}
	}
	return filepath.Clean(filepath.Join(home, path))
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// CleanExpiredTempDirs 清理目录下超过指定时长的子文件与目录
func CleanExpiredTempDirs(tmpBase string, maxAge time.Duration) {
	entries, err := os.ReadDir(tmpBase)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			_ = os.RemoveAll(filepath.Join(tmpBase, entry.Name()))
		}
	}
}

func (h *Handler) handlePhoneLogin(ctx context.Context, openID, msgID, content string) {
	cfg := h.cfgGetter.Get()
	ncmmExe := cfg.NCMMExe
	if ncmmExe == "" {
		h.replyText(ctx, openID, errNoNcmmExe, msgID)
		return
	}

	phone, ctcode, target, err := parsePhoneArgs(content)
	if err != nil {
		h.replyText(ctx, openID, "💡 手机登录用法：\n/phone <手机号> [main/文件名] [--ctcode 区号]\n\n示例:\n• /phone 13800138000\n• /phone 13800138000 main\n• /phone 13800138000 fan1.json\n• /phone +85298765432", msgID)
		return
	}

	h.replyText(ctx, openID, fmt.Sprintf("📲 正在向 +%d %s 请求发送短信验证码，请稍候...", ctcode, maskPhone(phone)), msgID)

	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, ncmmExe, "login", "phone", phone, "--countrycode", strconv.FormatInt(ctcode, 10), "--send-only")
	cmd.Dir = cfg.NCMMHome

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		errInfo := errBuf.String()
		if errInfo == "" {
			errInfo = outBuf.String()
		}
		h.replyText(ctx, openID, fmt.Sprintf("❌ 发送短信验证码失败: %v\n%s", err, strings.TrimSpace(errInfo)), msgID)
		return
	}

	session := &PhoneLoginSession{
		OpenID:      openID,
		Cellphone:   phone,
		CountryCode: ctcode,
		Target:      target,
		Step:        0,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	}
	h.phoneMu.Lock()
	h.phoneSessions[openID] = session
	h.phoneMu.Unlock()

	targetDesc := ""
	if target != "" {
		targetDesc = fmt.Sprintf("\n(登录成功后将绑定至: %s)", target)
	}

	reply := fmt.Sprintf("📲 短信验证码已成功发送至 +%d %s！%s\n\n请直接回复收到的 4~6 位数字验证码（如直接发送 123456 或 /code 123456）。\n⏰ 验证码 5 分钟内有效，回复 /cancel 可随时取消。",
		ctcode, maskPhone(phone), targetDesc)
	h.replyText(ctx, openID, reply, msgID)
}

func (h *Handler) processPhoneSessionInput(ctx context.Context, openID, msgID, content string) bool {
	h.phoneMu.Lock()
	session, ok := h.phoneSessions[openID]
	if !ok {
		h.phoneMu.Unlock()
		return false
	}
	if time.Now().After(session.ExpiresAt) {
		delete(h.phoneSessions, openID)
		h.phoneMu.Unlock()
		h.replyText(ctx, openID, "⏰ 手机验证码登录会话已超时失效，请重新发送 /phone 发起登录。", msgID)
		return false
	}
	h.phoneMu.Unlock()

	trimmed := strings.TrimSpace(content)
	lower := strings.ToLower(trimmed)

	if lower == "/cancel" || lower == "cancel" || lower == "取消" {
		h.phoneMu.Lock()
		delete(h.phoneSessions, openID)
		h.phoneMu.Unlock()
		h.replyText(ctx, openID, "🛑 已取消本次手机验证码登录。", msgID)
		return true
	}

	// 若用户输入以斜杠开头且不是 /code 或 /name，检查是否是取消并切换其他指令
	if strings.HasPrefix(lower, "/") && !strings.HasPrefix(lower, "/code") && !strings.HasPrefix(lower, "/name") {
		if strings.HasPrefix(lower, "/phone") || strings.HasPrefix(lower, "/login") {
			h.phoneMu.Lock()
			delete(h.phoneSessions, openID)
			h.phoneMu.Unlock()
			return false
		}
		return false
	}

	if session.Step == 0 {
		captcha := trimmed
		if strings.HasPrefix(lower, "/code ") {
			captcha = strings.TrimSpace(trimmed[6:])
		} else if strings.HasPrefix(lower, "code ") {
			captcha = strings.TrimSpace(trimmed[5:])
		}
		if len(captcha) < 4 || len(captcha) > 8 {
			return false
		}
		h.handlePhoneVerifyCaptcha(ctx, openID, msgID, session, captcha)
		return true
	} else if session.Step == 1 {
		nickname := trimmed
		if strings.HasPrefix(lower, "/name ") {
			nickname = strings.TrimSpace(trimmed[6:])
		} else if strings.HasPrefix(lower, "name ") {
			nickname = strings.TrimSpace(trimmed[5:])
		}
		if nickname == "" {
			h.replyText(ctx, openID, "⚠️ 昵称不能为空，请回复希望设置的新账号昵称（或回复 /cancel 取消）。", msgID)
			return true
		}
		h.handlePhoneSubmitNickname(ctx, openID, msgID, session, nickname)
		return true
	}

	return false
}

func (h *Handler) handlePhoneVerifyCaptcha(ctx context.Context, openID, msgID string, session *PhoneLoginSession, captcha string) {
	cfg := h.cfgGetter.Get()
	ncmmExe := cfg.NCMMExe

	h.replyText(ctx, openID, "⏳ 正在校验验证码并登录网易云音乐，请稍候...", msgID)

	args := []string{
		"login", "phone", session.Cellphone,
		"--countrycode", strconv.FormatInt(session.CountryCode, 10),
		"--captcha", captcha,
		"--json-result",
	}

	if session.Target != "" {
		if strings.EqualFold(session.Target, "main") || session.Target == "主" || session.Target == "主账号" {
			args = append(args, "--main")
		} else {
			targetFile := session.Target
			if !strings.HasSuffix(strings.ToLower(targetFile), ".json") {
				targetFile += ".json"
			}
			args = append(args, "-o", targetFile)
		}
	}

	cmdCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, ncmmExe, args...)
	cmd.Dir = cfg.NCMMHome

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	outStr := outBuf.String()
	errStr := errBuf.String()
	combinedOut := outStr + "\n" + errStr

	// 检查是否二次放号/新号注册 (code: 8860)
	if strings.Contains(combinedOut, "8860") || strings.Contains(combinedOut, "二次放号") {
		session.Step = 1
		session.Captcha = captcha
		session.ExpiresAt = time.Now().Add(5 * time.Minute)
		h.replyText(ctx, openID, "⚠️ 检测到该号码为未注册新号或二次放号，请直接回复希望设置的新账号昵称（如直接发送 听歌达人 或 /name 听歌达人）。", msgID)
		return
	}

	h.phoneMu.Lock()
	delete(h.phoneSessions, openID)
	h.phoneMu.Unlock()

	if err != nil {
		h.replyText(ctx, openID, fmt.Sprintf("❌ 登录失败: %v\n%s", err, cleanOutput(errStr, outStr)), msgID)
		return
	}

	res, ok := parseLoginResult(outStr)
	if !ok || res.UID == 0 {
		h.replyText(ctx, openID, "🎉 登录命令已执行完成:\n"+cleanOutput(outStr, errStr), msgID)
		return
	}

	accType := "【辅助账号】"
	if res.Main {
		accType = "【主账号】"
	}

	reply := fmt.Sprintf("🎉 手机验证码登录成功！\n------------------------\n身份类型: %s\n用户昵称: %s\n用户 UID: %d\n落盘位置: %s",
		accType, res.Nickname, res.UID, res.AccountPath)
	h.replyText(ctx, openID, reply, msgID)
}

func (h *Handler) handlePhoneSubmitNickname(ctx context.Context, openID, msgID string, session *PhoneLoginSession, nickname string) {
	cfg := h.cfgGetter.Get()
	ncmmExe := cfg.NCMMExe

	h.replyText(ctx, openID, fmt.Sprintf("⏳ 正在为手机号注册新账号 (昵称: %s)，请稍候...", nickname), msgID)

	args := []string{
		"login", "phone", session.Cellphone,
		"--countrycode", strconv.FormatInt(session.CountryCode, 10),
		"--captcha", session.Captcha,
		"--nickname", nickname,
		"--json-result",
	}

	if session.Target != "" {
		if strings.EqualFold(session.Target, "main") || session.Target == "主" || session.Target == "主账号" {
			args = append(args, "--main")
		} else {
			targetFile := session.Target
			if !strings.HasSuffix(strings.ToLower(targetFile), ".json") {
				targetFile += ".json"
			}
			args = append(args, "-o", targetFile)
		}
	}

	cmdCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, ncmmExe, args...)
	cmd.Dir = cfg.NCMMHome

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	outStr := outBuf.String()
	errStr := errBuf.String()

	h.phoneMu.Lock()
	delete(h.phoneSessions, openID)
	h.phoneMu.Unlock()

	if err != nil {
		h.replyText(ctx, openID, fmt.Sprintf("❌ 注册新账号失败: %v\n%s", err, cleanOutput(errStr, outStr)), msgID)
		return
	}

	res, ok := parseLoginResult(outStr)
	if !ok || res.UID == 0 {
		h.replyText(ctx, openID, "🎉 注册命令已完成:\n"+cleanOutput(outStr, errStr), msgID)
		return
	}

	accType := "【辅助账号】"
	if res.Main {
		accType = "【主账号】"
	}

	reply := fmt.Sprintf("🎉 手机新账号注册并登录成功！\n------------------------\n身份类型: %s\n用户昵称: %s\n用户 UID: %d\n落盘位置: %s",
		accType, res.Nickname, res.UID, res.AccountPath)
	h.replyText(ctx, openID, reply, msgID)
}

func (h *Handler) handleCancelSession(ctx context.Context, openID, msgID string) {
	h.phoneMu.Lock()
	_, exists := h.phoneSessions[openID]
	if exists {
		delete(h.phoneSessions, openID)
		h.phoneMu.Unlock()
		h.replyText(ctx, openID, "🛑 已取消正在进行中的手机验证码登录会话。", msgID)
		return
	}
	h.phoneMu.Unlock()
	h.replyText(ctx, openID, "ℹ️ 当前没有进行中的会话。", msgID)
}

func parsePhoneArgs(content string) (phone string, ctcode int64, target string, err error) {
	ctcode = 86
	content = strings.TrimSpace(content)
	var rest string
	lower := strings.ToLower(content)
	if strings.HasPrefix(lower, "/login phone") {
		rest = strings.TrimSpace(content[len("/login phone"):])
	} else if strings.HasPrefix(lower, "/phone") {
		rest = strings.TrimSpace(content[len("/phone"):])
	} else {
		rest = content
	}

	parts := strings.Fields(rest)
	if len(parts) == 0 {
		return "", 0, "", fmt.Errorf("请输入手机号码，例如: /phone 13800138000")
	}

	rawPhone := parts[0]
	if strings.HasPrefix(rawPhone, "+") {
		trimmed := strings.TrimPrefix(rawPhone, "+")
		if strings.HasPrefix(trimmed, "86") && len(trimmed) > 8 {
			ctcode = 86
			phone = trimmed[2:]
		} else if strings.HasPrefix(trimmed, "852") && len(trimmed) > 6 {
			ctcode = 852
			phone = trimmed[3:]
		} else if strings.HasPrefix(trimmed, "886") && len(trimmed) > 6 {
			ctcode = 886
			phone = trimmed[3:]
		} else if strings.HasPrefix(trimmed, "1") && len(trimmed) > 7 {
			ctcode = 1
			phone = trimmed[1:]
		} else {
			phone = trimmed
		}
	} else {
		phone = rawPhone
	}

	phone = strings.ReplaceAll(phone, "-", "")
	phone = strings.ReplaceAll(phone, " ", "")

	for i := 1; i < len(parts); i++ {
		arg := parts[i]
		if arg == "--ctcode" || arg == "--countrycode" || arg == "-c" {
			if i+1 < len(parts) {
				cc, e := strconv.ParseInt(parts[i+1], 10, 64)
				if e == nil && cc > 0 {
					ctcode = cc
				}
				i++
			}
		} else if strings.EqualFold(arg, "main") || arg == "主" || arg == "主账号" {
			target = "main"
		} else if strings.HasSuffix(strings.ToLower(arg), ".json") {
			target = arg
		} else if cc, e := strconv.ParseInt(arg, 10, 64); e == nil && len(arg) <= 4 {
			ctcode = cc
		} else {
			target = arg
		}
	}

	if len(phone) < 5 {
		return "", 0, "", fmt.Errorf("手机号码格式不正确: %s", phone)
	}

	return phone, ctcode, target, nil
}

func maskPhone(phone string) string {
	if len(phone) >= 7 {
		return phone[:3] + "****" + phone[len(phone)-4:]
	}
	return phone
}

func splitCommandArgs(s string) ([]string, error) {
	var args []string
	var current strings.Builder
	inDoubleQuotes := false
	inSingleQuotes := false
	escaped := false

	for _, r := range s {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' && !inSingleQuotes {
			inDoubleQuotes = !inDoubleQuotes
			continue
		}
		if r == '\'' && !inDoubleQuotes {
			inSingleQuotes = !inSingleQuotes
			continue
		}
		if (r == ' ' || r == '\t') && !inDoubleQuotes && !inSingleQuotes {
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(r)
	}
	if inDoubleQuotes || inSingleQuotes {
		return nil, fmt.Errorf("命令行引号未闭合")
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args, nil
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripAnsi(str string) string {
	return ansiRegex.ReplaceAllString(str, "")
}

func getLastLines(str string, n int) string {
	str = strings.ReplaceAll(str, "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(str, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func parseLoginResult(out string) (LoginJSONResult, bool) {
	var res LoginJSONResult
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "NCMM_LOGIN_RESULT")
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, "{"); idx >= 0 && strings.HasSuffix(line, "}") {
			jsonStr := line[idx:]
			if json.Unmarshal([]byte(jsonStr), &res) == nil && res.UID > 0 {
				return res, true
			}
		}
	}
	return res, false
}

func cleanOutput(primary, fallback string) string {
	s := strings.TrimSpace(primary)
	if s == "" {
		s = strings.TrimSpace(fallback)
	}
	s = stripAnsi(s)
	if len(s) > 500 {
		s = s[:500] + "\n...(输出过长已截断)"
	}
	return s
}
