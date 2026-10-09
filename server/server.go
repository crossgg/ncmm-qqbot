package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crossgg/ncmm-qqbot/bot"
)

type ConfigManager interface {
	Get() ConfigDTO
	Save(*ConfigDTO) error
}

type ConfigDTO struct {
	AutoNotify   *bool  `json:"auto_notify"`
	AppID        string `json:"app_id"`
	ClientSecret string `json:"client_secret"`
	AdminOpenID  string `json:"admin_openid"`
	Port         int    `json:"port"`
	Host         string `json:"host"`
	NCMMHome     string `json:"ncmm_home"`
	NCMMExe      string `json:"ncmm_exe"`
}

type GatewayStatusGetter interface {
	GetStatus() (bot.GatewayStatus, string)
	Restart(ctx context.Context)
}

type NotifyPayload struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Level   string `json:"level"`
	Time    string `json:"time"`
	Host    string `json:"host"`
	Msg     string `json:"msg"`
	Text    string `json:"text"`
}

type Server struct {
	cm      ConfigManager
	botCli  *bot.Client
	gw      GatewayStatusGetter
	srv     *http.Server
	mu      sync.Mutex
	history []NotifyHistory
}

type NotifyHistory struct {
	Time    string `json:"time"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

func NewServer(cm ConfigManager, botCli *bot.Client, gw GatewayStatusGetter) *Server {
	return &Server{
		cm:      cm,
		botCli:  botCli,
		gw:      gw,
		history: make([]NotifyHistory, 0, 50),
	}
}

func (s *Server) Start(ctx context.Context) error {
	cfg := s.cm.Get()
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/notify", s.handleNotify)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/config", s.handleAPIConfig)
	mux.HandleFunc("/api/test_token", s.handleAPITestToken)
	mux.HandleFunc("/api/test_notify", s.handleAPITestNotify)

	s.srv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	log.Printf("[QQBot-Web] 正在启动 Web 控制台与 Notify 监听服务: http://%s", addr)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
	}()

	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	}
	return nil
}

func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Read body failed", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var payload NotifyPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		payload.Content = string(body)
	}

	title := payload.Title
	if title == "" {
		title = "NCMM 任务推送"
	}
	content := payload.Content
	if content == "" {
		if payload.Msg != "" {
			content = payload.Msg
		} else if payload.Text != "" {
			content = payload.Text
		} else {
			content = string(body)
		}
	}

	cfg := s.cm.Get()
	adminID := strings.TrimSpace(cfg.AdminOpenID)
	if adminID == "" {
		log.Printf("[QQBot-Notify] 收到推送通知但未配置管理员 OpenID，无法发送私信: %s", title)
		s.recordHistory(title, content, "未配置管理员")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"skipped","reason":"no admin_openid"}`))
		return
	}

	// 格式化 QQ 私聊消息
	icon := "📢"
	if strings.Contains(strings.ToLower(payload.Level), "error") || strings.Contains(title, "失败") || strings.Contains(title, "异常") {
		icon = "⚠️"
	}
	timeStr := time.Now().Format("2006-01-02 15:04:05")

	formattedMsg := fmt.Sprintf("%s【%s】\n时间: %s\n------------------------\n%s",
		icon, title, timeStr, content)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, sendErr := s.botCli.SendC2CMessage(ctx, adminID, formattedMsg, "")
	if sendErr != nil {
		log.Printf("[QQBot-Notify] 推送消息给管理员失败: %v", sendErr)
		s.recordHistory(title, content, "推送失败: "+sendErr.Error())
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","error":"` + sendErr.Error() + `"}`))
		return
	}

	log.Printf("[QQBot-Notify] 成功推送通知至管理员 QQ: %s", title)
	s.recordHistory(title, content, "推送成功")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) recordHistory(title, content, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := NotifyHistory{
		Time:    time.Now().Format("15:04:05"),
		Title:   title,
		Content: content,
		Status:  status,
	}
	if len(s.history) >= 50 {
		s.history = s.history[1:]
	}
	s.history = append(s.history, h)
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.cm.Get()
	status, lastErr := s.gw.GetStatus()

	s.mu.Lock()
	hist := make([]NotifyHistory, len(s.history))
	copy(hist, s.history)
	s.mu.Unlock()

	resp := map[string]interface{}{
		"config":        cfg,
		"status":        status,
		"last_error":    lastErr,
		"notify_count":  len(hist),
		"history":       hist,
		"has_admin":     cfg.AdminOpenID != "",
		"has_auth":      cfg.AppID != "" && cfg.ClientSecret != "",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ConfigDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Port <= 0 {
		req.Port = 5606
	}
	if req.Host == "" {
		req.Host = "0.0.0.0"
	}
	if req.AutoNotify == nil {
		defTrue := true
		req.AutoNotify = &defTrue
	}

	if err := s.cm.Save(&req); err != nil {
		http.Error(w, "保存配置失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 更新客户端凭证
	s.botCli.UpdateCredentials(req.AppID, req.ClientSecret)
	// 触发网关重启连接
	s.gw.Restart(context.Background())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "配置已成功保存并生效！",
	})
}

func (s *Server) handleAPITestToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		AppID        string `json:"app_id"`
		ClientSecret string `json:"client_secret"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	testCli := s.botCli
	if req.AppID != "" && req.ClientSecret != "" {
		testCli = bot.NewClient(req.AppID, req.ClientSecret)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	gw, err := testCli.TestCredentials(ctx)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "凭证有效，成功连接腾讯开放平台网关！",
		"gateway": gw,
	})
}

func (s *Server) handleAPITestNotify(w http.ResponseWriter, r *http.Request) {
	cfg := s.cm.Get()
	adminID := strings.TrimSpace(cfg.AdminOpenID)
	if adminID == "" {
		http.Error(w, "尚未绑定管理员 OpenID", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	testContent := fmt.Sprintf("🔔【NCMM 测试通知】\n时间: %s\n------------------------\n这是一条来自 Web 控制台的测试推送消息。机器人通知链路工作完全正常！",
		time.Now().Format("2006-01-02 15:04:05"))

	_, err := s.botCli.SendC2CMessage(ctx, adminID, testContent, "")
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "测试通知已成功推送到管理员 QQ 私聊！",
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(htmlTemplate))
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>NCMM QQ 机器人配置管理面板</title>
  <style>
    :root {
      --bg: #0f172a;
      --card: #1e293b;
      --border: #334155;
      --text: #f8fafc;
      --text-muted: #94a3b8;
      --primary: #38bdf8;
      --primary-hover: #0284c7;
      --success: #22c55e;
      --warn: #eab308;
      --error: #ef4444;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
      background: var(--bg);
      color: var(--text);
      line-height: 1.6;
      padding: 24px;
    }
    .container { max-width: 900px; margin: 0 auto; }
    .header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-bottom: 24px;
      padding-bottom: 16px;
      border-bottom: 1px solid var(--border);
    }
    .header h1 { font-size: 22px; font-weight: 700; color: #fff; }
    .status-badge {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      padding: 6px 14px;
      border-radius: 9999px;
      font-size: 13px;
      font-weight: 600;
      background: #0f172a;
      border: 1px solid var(--border);
    }
    .dot { width: 10px; height: 10px; border-radius: 50%; background: #64748b; }
    .dot.connected { background: var(--success); box-shadow: 0 0 10px var(--success); }
    .dot.connecting { background: var(--warn); box-shadow: 0 0 10px var(--warn); }
    .dot.error { background: var(--error); }
    
    .grid { display: grid; grid-template-columns: 1fr; gap: 20px; }
    .card {
      background: var(--card);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 24px;
    }
    .card h2 { font-size: 17px; margin-bottom: 16px; color: var(--primary); display: flex; align-items: center; gap: 8px; }
    
    .form-group { margin-bottom: 18px; }
    label { display: block; font-size: 13px; font-weight: 600; margin-bottom: 6px; color: var(--text-muted); }
    input[type="text"], input[type="password"], input[type="number"] {
      width: 100%;
      padding: 10px 14px;
      background: #0f172a;
      border: 1px solid var(--border);
      border-radius: 8px;
      color: #fff;
      font-size: 14px;
      transition: border 0.2s;
    }
    input:focus { outline: none; border-color: var(--primary); }
    .help-text { font-size: 12px; color: var(--text-muted); margin-top: 4px; }
    
    .btn-group { display: flex; gap: 12px; margin-top: 24px; flex-wrap: wrap; }
    button {
      padding: 10px 20px;
      border-radius: 8px;
      font-size: 14px;
      font-weight: 600;
      cursor: pointer;
      border: none;
      transition: all 0.2s;
    }
    .btn-primary { background: var(--primary); color: #0f172a; }
    .btn-primary:hover { background: var(--primary-hover); }
    .btn-secondary { background: #334155; color: #fff; }
    .btn-secondary:hover { background: #475569; }
    
    .tip-box {
      background: rgba(56, 189, 248, 0.08);
      border-left: 4px solid var(--primary);
      padding: 12px 16px;
      border-radius: 6px;
      font-size: 13px;
      margin-bottom: 20px;
    }
    
    .history-table { width: 100%; border-collapse: collapse; font-size: 13px; margin-top: 10px; }
    .history-table th, .history-table td { padding: 10px 12px; border-bottom: 1px solid var(--border); text-align: left; }
    .history-table th { color: var(--text-muted); font-weight: 600; }
    
    #toast {
      position: fixed;
      bottom: 24px;
      right: 24px;
      padding: 12px 20px;
      border-radius: 8px;
      background: #334155;
      color: #fff;
      font-size: 14px;
      display: none;
      box-shadow: 0 4px 12px rgba(0,0,0,0.4);
      z-index: 1000;
    }

    .notify-toggle-card {
      background: #0f172a;
      padding: 14px 16px;
      border-radius: 8px;
      border: 1px solid var(--border);
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      margin-bottom: 20px;
    }
    .switch {
      position: relative;
      display: inline-block;
      width: 44px;
      height: 24px;
      flex-shrink: 0;
      margin: 0;
      cursor: pointer;
    }
    .switch input { opacity: 0; width: 0; height: 0; }
    .slider {
      position: absolute;
      cursor: pointer;
      top: 0; left: 0; right: 0; bottom: 0;
      background-color: #334155;
      transition: .3s;
      border-radius: 24px;
    }
    .slider:before {
      position: absolute;
      content: "";
      height: 18px;
      width: 18px;
      left: 3px;
      bottom: 3px;
      background-color: white;
      transition: .3s;
      border-radius: 50%;
    }
    .switch input:checked + .slider { background-color: var(--primary); }
    .switch input:checked + .slider:before { transform: translateX(20px); }
  </style>
</head>
<body>
  <div class="container">
    <div class="header">
      <div>
        <h1>🤖 NCMM QQ 机器人配置管理</h1>
        <p style="font-size: 13px; color: var(--text-muted); margin-top: 2px;">腾讯官方 QQ 机器人 API v2 · 本地守护与通知服务</p>
      </div>
      <div class="status-badge" id="statusBadge">
        <div class="dot" id="statusDot"></div>
        <span id="statusText">正在检查...</span>
      </div>
    </div>

    <div class="tip-box">
      <strong>💡 快速指引：</strong>
      请填入在 <a href="https://bot.q.qq.com" target="_blank" style="color: var(--primary);">QQ 开放平台</a> 创建的机器人凭证。
      初次使用时，在 QQ 私信机器人发送 <code>/myid</code> 即可获取您的专属 OpenID，随后填入下方或直接私聊回复 <code>/bind</code> 绑定。
    </div>

    <div class="grid">
      <div class="card">
        <h2>⚙️ 核心参数在线配置</h2>
        <form id="cfgForm">
          <div class="notify-toggle-card">
            <div>
              <label for="auto_notify" style="margin: 0; font-size: 14px; font-weight: 600; color: #fff; cursor: pointer;">
                🔔 开启系统通知推送
              </label>
              <div class="help-text" style="margin-top: 2px;">自动在宿主 notify.yaml 中配置并启用 Webhook，端口与下方保持一致。</div>
            </div>
            <label class="switch">
              <input type="checkbox" id="auto_notify" checked>
              <span class="slider"></span>
            </label>
          </div>

          <div class="form-group">
            <label>QQ 机器人 AppID <span style="color:var(--error)">*</span></label>
            <input type="text" id="app_id" placeholder="例如: 102030405" required>
            <div class="help-text">在 QQ 开放平台 - 应用管理 - 机器人设置中查看。</div>
          </div>

          <div class="form-group">
            <label>AppSecret / ClientSecret <span style="color:var(--error)">*</span></label>
            <input type="password" id="client_secret" placeholder="例如: 32位密钥字符串" required>
            <div class="help-text">开放平台生成的密钥（本地加密存储）。</div>
          </div>

          <div class="form-group">
            <label>管理员 QQ OpenID</label>
            <input type="text" id="admin_openid" placeholder="例如: 8A29F4E567B10...">
            <div class="help-text">管理员身份凭据。私聊机器人发送 <code>/myid</code> 可直接查询。</div>
          </div>

          <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 16px;">
            <div class="form-group">
              <label>服务端口 (Port)</label>
              <input type="number" id="port" value="5606">
              <div class="help-text">Notify 接收与本面板端口。</div>
            </div>
            <div class="form-group">
              <label>监听地址 (Host)</label>
              <input type="text" id="host" value="0.0.0.0">
              <div class="help-text">默认 0.0.0.0 支持局域网访问。</div>
            </div>
          </div>

          <div class="form-group">
            <label>NCMM 宿主主目录 (NCMM_HOME)</label>
            <input type="text" id="ncmm_home" placeholder="自动探测">
            <div class="help-text">程序会自动识别，一般无需手动修改。</div>
          </div>

          <div class="btn-group">
            <button type="submit" class="btn-primary">💾 保存配置并应用</button>
            <button type="button" class="btn-secondary" id="btnTestToken">⚡ 测试凭证连接</button>
            <button type="button" class="btn-secondary" id="btnTestNotify">📨 发送测试通知</button>
          </div>
        </form>
      </div>

      <div class="card">
        <h2>📋 通知中继与对接说明</h2>
        <p style="font-size: 13px; color: var(--text-muted); margin-bottom: 12px;">
          开启上方【开启系统通知推送】后，插件会自动在宿主 <code>notify.yaml</code> 中写入并激活 Webhook，端口与配置实时同步：
        </p>
        <pre style="background: #0f172a; padding: 14px; border-radius: 8px; font-size: 12px; color: #38bdf8; overflow-x: auto;" id="webhookPreview">
webhook:
  enabled: true
  url: "http://127.0.0.1:5606/notify"
  method: POST
        </pre>
      </div>

      <div class="card">
        <h2>📜 最近消息推送日志</h2>
        <div id="historyContainer" style="overflow-x: auto;">
          <table class="history-table">
            <thead>
              <tr><th>时间</th><th>标题</th><th>状态</th></tr>
            </thead>
            <tbody id="historyBody">
              <tr><td colspan="3" style="text-align:center; color: var(--text-muted);">暂无推送记录</td></tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>
  </div>

  <div id="toast"></div>

  <script>
    function showToast(msg, isErr) {
      const t = document.getElementById('toast');
      t.innerText = msg;
      t.style.background = isErr ? '#ef4444' : '#22c55e';
      t.style.display = 'block';
      setTimeout(() => { t.style.display = 'none'; }, 3000);
    }

    async function fetchStatus() {
      try {
        const res = await fetch('/api/status');
        const data = await res.json();
        
        // 更新表单
        if (data.config) {
          document.getElementById('auto_notify').checked = data.config.auto_notify !== false;
          document.getElementById('app_id').value = data.config.app_id || '';
          document.getElementById('client_secret').value = data.config.client_secret || '';
          document.getElementById('admin_openid').value = data.config.admin_openid || '';
          document.getElementById('port').value = data.config.port || 5606;
          document.getElementById('host').value = data.config.host || '0.0.0.0';
          document.getElementById('ncmm_home').value = data.config.ncmm_home || '';
          updateWebhookPreview();
        }

        // 状态徽标
        const dot = document.getElementById('statusDot');
        const txt = document.getElementById('statusText');
        dot.className = 'dot ' + data.status;
        if (data.status === 'connected') {
          txt.innerText = '网关已连接 (监听中)';
        } else if (data.status === 'connecting') {
          txt.innerText = '正在连接网关...';
        } else if (data.status === 'error') {
          txt.innerText = '连接异常: ' + (data.last_error || '鉴权失败');
        } else {
          txt.innerText = data.has_auth ? '未连接' : '未配置凭据';
        }

        // 历史日志
        const tbody = document.getElementById('historyBody');
        if (data.history && data.history.length > 0) {
          tbody.innerHTML = data.history.map(function(h) {
            var color = (h.status && h.status.indexOf('成功') !== -1) ? '#22c55e' : '#ef4444';
            return '<tr><td>' + h.time + '</td><td>' + h.title + '</td><td style="color:' + color + '">' + h.status + '</td></tr>';
          }).reverse().join('');
        }
      } catch (e) {
        console.error(e);
      }
    }

    function updateWebhookPreview() {
      const p = parseInt(document.getElementById('port').value) || 5606;
      const isChecked = document.getElementById('auto_notify').checked;
      const el = document.getElementById('webhookPreview');
      if (el) {
        el.innerText = 'webhook:\n  enabled: ' + isChecked + '\n  url: "http://127.0.0.1:' + p + '/notify"\n  method: POST';
      }
    }
    document.getElementById('port').addEventListener('input', updateWebhookPreview);
    document.getElementById('auto_notify').addEventListener('change', updateWebhookPreview);

    document.getElementById('cfgForm').addEventListener('submit', async (e) => {
      e.preventDefault();
      const payload = {
        auto_notify: document.getElementById('auto_notify').checked,
        app_id: document.getElementById('app_id').value.trim(),
        client_secret: document.getElementById('client_secret').value.trim(),
        admin_openid: document.getElementById('admin_openid').value.trim(),
        port: parseInt(document.getElementById('port').value) || 5606,
        host: document.getElementById('host').value.trim() || '0.0.0.0',
        ncmm_home: document.getElementById('ncmm_home').value.trim(),
      };

      try {
        const res = await fetch('/api/config', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        const d = await res.json();
        if (d.success) {
          showToast('✅ 配置保存成功！正在重启网关连接...', false);
          setTimeout(fetchStatus, 1500);
        } else {
          showToast('❌ 保存失败: ' + (d.error || '未知错误'), true);
        }
      } catch (e) {
        showToast('❌ 网络错误: ' + e.message, true);
      }
    });

    document.getElementById('btnTestToken').addEventListener('click', async () => {
      showToast('正在测试凭据连接...', false);
      try {
        const res = await fetch('/api/test_token', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            app_id: document.getElementById('app_id').value.trim(),
            client_secret: document.getElementById('client_secret').value.trim(),
          })
        });
        const d = await res.json();
        if (d.success) {
          showToast('🎉 ' + d.message, false);
        } else {
          showToast('❌ ' + (d.error || '连接失败'), true);
        }
      } catch (e) {
        showToast('❌ 测试失败: ' + e.message, true);
      }
    });

    document.getElementById('btnTestNotify').addEventListener('click', async () => {
      try {
        const res = await fetch('/api/test_notify', { method: 'POST' });
        const d = await res.json();
        if (d.success) {
          showToast('✅ 测试消息已发送至管理员 QQ！', false);
          setTimeout(fetchStatus, 1000);
        } else {
          showToast('❌ 发送失败: ' + (d.error || '未知错误'), true);
        }
      } catch (e) {
        showToast('❌ 网络错误: ' + e.message, true);
      }
    });

    fetchStatus();
    setInterval(fetchStatus, 5000);
  </script>
</body>
</html>
`
