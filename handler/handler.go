package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	AppID        string
	ClientSecret string
	AdminOpenID  string
	Port         int
	Host         string
	NCMMHome     string
	NCMMExe      string
}

type Handler struct {
	client     *bot.Client
	cfgGetter  ConfigGetter
	loginMu    sync.Mutex
	isLogining bool
}

func NewHandler(client *bot.Client, cfgGetter ConfigGetter) *Handler {
	return &Handler{
		client:    client,
		cfgGetter: cfgGetter,
	}
}

func (h *Handler) HandleMessage(ctx context.Context, msg *bot.C2CMessage) {
	senderOpenID := msg.Author.UserOpenID
	content := strings.TrimSpace(msg.Content)
	msgID := msg.ID

	cfg := h.cfgGetter.Get()
	adminID := strings.TrimSpace(cfg.AdminOpenID)

	// 1. 处理无需管理员权限的基础指令
	if strings.EqualFold(content, "/myid") || strings.EqualFold(content, "id") {
		resp := fmt.Sprintf("您的 QQ OpenID 为：\n%s\n\n", senderOpenID)
		if adminID == "" {
			resp += "⚠️ 当前系统尚未绑定管理员。\n您可以回复 /bind 将此账号设为管理员，或在 Web 管理面板在线保存。"
		} else if adminID == senderOpenID {
			resp += "✅ 您是当前系统的认证管理员。"
		} else {
			resp += "🔒 您不是当前系统的管理员。"
		}
		_, _ = h.client.SendC2CMessage(ctx, senderOpenID, resp, msgID)
		return
	}

	// 2. 绑定管理员指令
	if strings.EqualFold(content, "/bind") {
		if adminID != "" && adminID != senderOpenID {
			_, _ = h.client.SendC2CMessage(ctx, senderOpenID, "❌ 系统已绑定其他管理员，如需重置请访问 Web 面板。", msgID)
			return
		}
		newCfg := cfg
		newCfg.AdminOpenID = senderOpenID
		if err := h.cfgGetter.Save(&newCfg); err != nil {
			_, _ = h.client.SendC2CMessage(ctx, senderOpenID, "❌ 绑定管理员失败: "+err.Error(), msgID)
			return
		}
		_, _ = h.client.SendC2CMessage(ctx, senderOpenID, "🎉 恭喜！已成功将您绑定为管理员！\n发送 /help 查看所有可用指令。", msgID)
		return
	}

	// 3. 管理员鉴权：若已设置管理员且非管理员发来消息，则拒绝执行敏感指令
	if adminID != "" && adminID != senderOpenID {
		_, _ = h.client.SendC2CMessage(ctx, senderOpenID, "🔒 权限不足：只有管理员可使用此机器人的控制功能。", msgID)
		return
	}

	// 4. 指令路由
	lower := strings.ToLower(content)
	switch {
	case lower == "/help" || lower == "help" || lower == "帮助" || lower == "/菜单":
		h.handleHelp(ctx, senderOpenID, msgID)

	case lower == "/status" || lower == "/info" || lower == "状态":
		h.handleStatus(ctx, senderOpenID, msgID)

	case lower == "/login" || lower == "/qrcode" || lower == "扫码" || strings.HasPrefix(lower, "/login ") || strings.HasPrefix(lower, "/qrcode "):
		h.handleQrcodeLogin(ctx, senderOpenID, msgID, content)

	case lower == "/run" || strings.HasPrefix(lower, "/run ") || lower == "运行" || lower == "打卡":
		h.handleRunTask(ctx, senderOpenID, msgID, content)

	case strings.HasPrefix(lower, "/cookie") || isCookieString(content):
		h.handleCookieUpdate(ctx, senderOpenID, msgID, content)

	default:
		reply := "🤖 收到您的指令。发送 /help 可查看功能菜单，发送 /myid 可查看身份标识。"
		_, _ = h.client.SendC2CMessage(ctx, senderOpenID, reply, msgID)
	}
}

func (h *Handler) handleHelp(ctx context.Context, openID, msgID string) {
	menu := `📖【NCMM QQ 机器人功能菜单】
------------------------
🔑 Cookie 更新与登录：
• 直接发送 Cookie 文本：自动识别账号并更新
• /login 或 /qrcode：获取网易云扫码登录二维码
• /cookie <账号别名> <内容>：指定小号更新

⚡ 控制与查询：
• /status ：查看各账号状态与配置文件
• /run    ：立即触发日常打卡任务
• /myid   ：查看我的 OpenID 标识
• /help   ：查看本帮助菜单

🌐 管理面板：访问本插件 Web 页面在线维护配置。`
	_, _ = h.client.SendC2CMessage(ctx, openID, menu, msgID)
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
		_, _ = h.client.SendC2CMessage(ctx, openID, fmt.Sprintf("⚠️ 未在 %s 找到 config.yaml 配置文件", ncmmHome), msgID)
		return
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		_, _ = h.client.SendC2CMessage(ctx, openID, "读取 config.yaml 失败: "+err.Error(), msgID)
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

	_, _ = h.client.SendC2CMessage(ctx, openID, sb.String(), msgID)
}

func (h *Handler) handleRunTask(ctx context.Context, openID, msgID, content string) {
	cfg := h.cfgGetter.Get()
	ncmmExe := cfg.NCMMExe
	if ncmmExe == "" {
		_, _ = h.client.SendC2CMessage(ctx, openID, "❌ 未找到 ncmm 可执行文件路径，请在 Web 面板中配置 ncmm_exe", msgID)
		return
	}

	args := []string{"task"}
	parts := strings.Fields(content)
	if len(parts) > 1 {
		args = append(args, parts[1:]...)
	}

	_, _ = h.client.SendC2CMessage(ctx, openID, fmt.Sprintf("🚀 开始执行命令: ncmm %s\n任务已在后台启动，请稍候...", strings.Join(args, " ")), msgID)

	go func() {
		cmdCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		cmd := exec.CommandContext(cmdCtx, ncmmExe, args...)
		cmd.Dir = cfg.NCMMHome

		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf

		err := cmd.Run()
		outStr := strings.TrimSpace(outBuf.String())
		if len(outStr) > 400 {
			outStr = outStr[:400] + "\n...(日志过长已截断)"
		}

		if err != nil {
			msg := fmt.Sprintf("❌ 任务执行完成但有报错:\n%v\n\n日志摘要:\n%s", err, outStr)
			_, _ = h.client.SendC2CMessage(context.Background(), openID, msg, "")
		} else {
			msg := fmt.Sprintf("✅ 任务执行成功完成！\n\n日志摘要:\n%s", outStr)
			_, _ = h.client.SendC2CMessage(context.Background(), openID, msg, "")
		}
	}()
}

type LoginJSONResult struct {
	UID         int64  `json:"uid"`
	Nickname    string `json:"nickname"`
	AvatarURL   string `json:"avatarUrl"`
	CookiePath  string `json:"cookiePath"`
	AccountPath string `json:"accountPath"`
	Main        bool   `json:"main"`
}

func (h *Handler) handleCookieUpdate(ctx context.Context, openID, msgID, content string) {
	cookieStr := extractCookieString(content)
	if cookieStr == "" {
		_, _ = h.client.SendC2CMessage(ctx, openID, "❌ 无法提取有效 Cookie，请确保包含 MUSIC_U 内容。", msgID)
		return
	}

	cfg := h.cfgGetter.Get()
	ncmmExe := cfg.NCMMExe
	if ncmmExe == "" {
		_, _ = h.client.SendC2CMessage(ctx, openID, "❌ 未配置 ncmm 可执行文件，无法更新 Cookie", msgID)
		return
	}

	// 1. 临时预校验：使用 --no-config-write 与 --json-result 探测该 Cookie 对应的 UID 与昵称
	_, _ = h.client.SendC2CMessage(ctx, openID, "🔍 正在连接网易云验证 Cookie 并自动识别账号身份...", msgID)

	probeCmd := exec.CommandContext(ctx, ncmmExe, "login", "cookie", "--no-config-write", "--json-result", cookieStr)
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
		_, _ = h.client.SendC2CMessage(ctx, openID, fmt.Sprintf("❌ Cookie 校验失败: %v\n%s", err, errInfo), msgID)
		return
	}

	var res LoginJSONResult
	// 找到 JSON 行
	lines := strings.Split(probeOut.String(), "\n")
	foundJSON := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			if json.Unmarshal([]byte(line), &res) == nil && res.UID > 0 {
				foundJSON = true
				break
			}
		}
	}

	if !foundJSON || res.UID == 0 {
		_, _ = h.client.SendC2CMessage(ctx, openID, "❌ 登录成功但无法解析账号信息: \n"+probeOut.String(), msgID)
		return
	}

	// 2. 自动匹配账号归属
	isMain, targetFile := h.detectAccountTarget(cfg.NCMMHome, res.UID)

	// 3. 执行正式回写命令
	var finalArgs []string
	if isMain {
		finalArgs = []string{"login", "cookie", "--main", cookieStr}
	} else if targetFile != "" {
		finalArgs = []string{"login", "cookie", "-o", targetFile, cookieStr}
	} else {
		finalArgs = []string{"login", "cookie", cookieStr}
	}

	saveCmd := exec.CommandContext(ctx, ncmmExe, finalArgs...)
	saveCmd.Dir = cfg.NCMMHome
	if err := saveCmd.Run(); err != nil {
		_, _ = h.client.SendC2CMessage(ctx, openID, "❌ 保存 Cookie 到系统失败: "+err.Error(), msgID)
		return
	}

	accountType := "【主账号】"
	if !isMain {
		accountType = "【辅助小号】"
	}

	reply := fmt.Sprintf("🎉 Cookie 自动识别并更新成功！\n------------------------\n身份类型: %s\n用户昵称: %s\n用户 UID: %d\n落盘位置: %s",
		accountType, res.Nickname, res.UID, res.AccountPath)
	_, _ = h.client.SendC2CMessage(ctx, openID, reply, msgID)
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
		_, _ = h.client.SendC2CMessage(ctx, openID, "⚠️ 当前已有一个扫码登录会话正在进行中，请在手机上确认或稍候重试。", msgID)
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
		_, _ = h.client.SendC2CMessage(ctx, openID, "❌ 未配置 ncmm 可执行文件，无法发起扫码登录", msgID)
		return
	}

	tempDir, err := os.MkdirTemp("", "ncmm-qq-qrcode-*")
	if err != nil {
		_, _ = h.client.SendC2CMessage(ctx, openID, "创建临时目录失败: "+err.Error(), msgID)
		return
	}
	defer os.RemoveAll(tempDir)

	_, _ = h.client.SendC2CMessage(ctx, openID, "📱 正在生成网易云音乐登录二维码，请稍候...", msgID)

	qrCmdCtx, qrCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer qrCancel()

	// 启动 ncmm login qrcode
	cmd := exec.CommandContext(qrCmdCtx, ncmmExe, "login", "qrcode", "-d", tempDir)
	cmd.Dir = cfg.NCMMHome

	var stdoutBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stdoutBuf

	if err := cmd.Start(); err != nil {
		_, _ = h.client.SendC2CMessage(ctx, openID, "❌ 启动扫码命令失败: "+err.Error(), msgID)
		return
	}

	qrPath := filepath.Join(tempDir, "qrcode.png")

	// 轮询等待二维码图片生成
	for i := 0; i < 50; i++ {
		time.Sleep(300 * time.Millisecond)
		if fileExists(qrPath) {
			imgBytes, err := os.ReadFile(qrPath)
			if err == nil && len(imgBytes) > 0 {
				// 优先直接发送图片
				sendImgErr := h.client.SendC2CImage(ctx, openID, imgBytes, msgID)
				if sendImgErr != nil {
					log.Printf("[QQBot-Qrcode] 发送二维码图片失败: %v，准备发送兜底文字链接", sendImgErr)
				}
				break
			}
		}
	}

	// 兜底文字链接
	time.Sleep(500 * time.Millisecond)
	outputSoFar := stdoutBuf.String()
	var codeKey string
	if re := regexp.MustCompile(`codekey=([a-zA-Z0-9-]+)`); re.MatchString(outputSoFar) {
		m := re.FindStringSubmatch(outputSoFar)
		if len(m) > 1 {
			codeKey = m[1]
		}
	}

	hintMsg := "👉 请使用【网易云音乐手机 App】扫码授权登录。\n有效时间 5 分钟，扫码确认后将自动更新凭据！"
	if codeKey != "" {
		hintMsg += fmt.Sprintf("\n\n若上方未显示图片，请点击扫码授权链接：\nhttps://music.163.com/login?codekey=%s", codeKey)
	}
	_, _ = h.client.SendC2CMessage(ctx, openID, hintMsg, msgID)

	// 等待扫码完成
	waitErr := cmd.Wait()
	if waitErr != nil {
		_, _ = h.client.SendC2CMessage(context.Background(), openID, "⏳ 扫码登录已超时或已取消。", "")
		return
	}

	out := stdoutBuf.String()
	nickname := "未知"
	if re := regexp.MustCompile(`nickname=([^\s\r\n]+)`); re.MatchString(out) {
		m := re.FindStringSubmatch(out)
		if len(m) > 1 {
			nickname = m[1]
		}
	}

	successMsg := fmt.Sprintf("🎉 扫码登录成功！\n账号【%s】Cookie 已更新并自动回写到系统配置中！", nickname)
	_, _ = h.client.SendC2CMessage(context.Background(), openID, successMsg, "")
}

func isCookieString(s string) bool {
	return strings.Contains(s, "MUSIC_U") || strings.Contains(s, "_ntes_nuid") || strings.Contains(s, "MUSIC_A_T")
}

func extractCookieString(content string) string {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(strings.ToLower(content), "/cookie") {
		parts := strings.Fields(content)
		if len(parts) >= 2 {
			// /cookie <content> 或 /cookie <alias> <content>
			if len(parts) >= 3 && !strings.Contains(parts[1], "=") {
				return strings.Join(parts[2:], " ")
			}
			return strings.Join(parts[1:], " ")
		}
	}
	return content
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
