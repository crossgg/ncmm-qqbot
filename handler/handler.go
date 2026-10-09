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

	// 5. 管理员指令路由
	// 5.1 优先检测是否附带了文件附件 (例如直接向机器人发送包含 Cookie 的 .txt/.json 文件)
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

	case lower == "/login" || lower == "/qrcode" || lower == "扫码" || strings.HasPrefix(lower, "/login ") || strings.HasPrefix(lower, "/qrcode "):
		h.handleQrcodeLogin(ctx, senderOpenID, msgID, content)

	case lower == "/run" || strings.HasPrefix(lower, "/run ") || lower == "运行" || lower == "打卡":
		h.handleRunTask(ctx, senderOpenID, msgID, content)

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
• /login 或 /qrcode  ：获取网易云登录授权链接（手机点击一键授权最便捷）
• 发送文件（强烈推荐）：直接发送包含 Cookie 的【.txt/.json】文件，秒级解析并绑定
• 发送文本          ：/cookie MUSIC_U=xxxxxx 或完整 Cookie
• 指定小号更新      ：/cookie <账号别名> MUSIC_U=xxxxxx

⚡ 控制与查询：
• /status ：查看各账号状态与配置文件
• /run    ：立即触发日常打卡任务
• /myid   ：查看我的 OpenID 标识
• /help   ：查看本帮助菜单

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
	cfg := h.cfgGetter.Get()
	ncmmExe := cfg.NCMMExe
	if ncmmExe == "" {
		h.replyText(ctx, openID, "❌ 未找到 ncmm 可执行文件路径，请在 Web 面板中配置 ncmm_exe", msgID)
		return
	}

	args := []string{"task"}
	parts := strings.Fields(content)
	if len(parts) > 1 {
		args = append(args, parts[1:]...)
	}

	h.replyText(ctx, openID, fmt.Sprintf("🚀 开始执行命令: ncmm %s\n任务已在后台启动，请稍候...", strings.Join(args, " ")), msgID)

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
			h.replyText(context.Background(), openID, msg, "")
		} else {
			msg := fmt.Sprintf("✅ 任务执行成功完成！\n\n日志摘要:\n%s", outStr)
			h.replyText(context.Background(), openID, msg, "")
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
		h.replyText(ctx, openID, "❌ 未配置 ncmm 可执行文件，无法更新 Cookie", msgID)
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

	var res LoginJSONResult
	// 找到 JSON 行 (兼容带 NCMM_LOGIN_RESULT 前缀输出)
	lines := strings.Split(probeOut.String(), "\n")
	foundJSON := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "NCMM_LOGIN_RESULT")
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, "{"); idx >= 0 && strings.HasSuffix(line, "}") {
			jsonStr := line[idx:]
			if json.Unmarshal([]byte(jsonStr), &res) == nil && res.UID > 0 {
				foundJSON = true
				break
			}
		}
	}

	if !foundJSON || res.UID == 0 {
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
		h.replyText(ctx, openID, "❌ 未配置 ncmm 可执行文件，无法发起扫码登录", msgID)
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
