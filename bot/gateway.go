package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type MessageAttachment struct {
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

type C2CMessage struct {
	ID          string              `json:"id"`
	Content     string              `json:"content"`
	Timestamp   string              `json:"timestamp"`
	Author      C2CAuthor           `json:"author"`
	Attachments []MessageAttachment `json:"attachments,omitempty"`
}

type C2CAuthor struct {
	UserOpenID string `json:"user_openid"`
	Username   string `json:"username"`
	Bot        bool   `json:"bot"`
}

type MessageHandler func(ctx context.Context, msg *C2CMessage)

type GatewayStatus string

const (
	StatusDisconnected GatewayStatus = "disconnected"
	StatusConnecting   GatewayStatus = "connecting"
	StatusConnected    GatewayStatus = "connected"
	StatusError        GatewayStatus = "error"
)

type GatewayManager struct {
	client  *Client
	handler MessageHandler

	mu        sync.RWMutex
	status    GatewayStatus
	lastError string
	sessionID string
	lastSeq   int64

	replyMu   sync.Mutex
	repliedAt map[string]time.Time

	cancelFunc context.CancelFunc
	running    bool
}

func NewGatewayManager(client *Client, handler MessageHandler) *GatewayManager {
	return &GatewayManager{
		client:    client,
		handler:   handler,
		status:    StatusDisconnected,
		repliedAt: make(map[string]time.Time),
	}
}

func (gm *GatewayManager) GetStatus() (GatewayStatus, string) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	return gm.status, gm.lastError
}

func (gm *GatewayManager) Start(ctx context.Context) {
	gm.mu.Lock()
	if gm.running {
		gm.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	gm.cancelFunc = cancel
	gm.running = true
	gm.mu.Unlock()

	go gm.runLoop(runCtx)
}

func (gm *GatewayManager) Stop() {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	if gm.cancelFunc != nil {
		gm.cancelFunc()
		gm.cancelFunc = nil
	}
	gm.running = false
	gm.status = StatusDisconnected
}

func (gm *GatewayManager) setStatus(s GatewayStatus, errStr string) {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	gm.status = s
	gm.lastError = errStr
}

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type helloData struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

type readyData struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	User      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

func (gm *GatewayManager) runLoop(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			gm.setStatus(StatusDisconnected, "")
			return
		default:
		}

		err := gm.connectAndListen(ctx)
		if err != nil {
			if ctx.Err() != nil {
				gm.setStatus(StatusDisconnected, "")
				return
			}
			log.Printf("[QQBot-Gateway] 连接断开: %v，%s 后自动重连...", err, backoff)
			gm.setStatus(StatusError, err.Error())
		} else {
			backoff = 2 * time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (gm *GatewayManager) markReply(messageID string) bool {
	if messageID == "" {
		return true
	}
	now := time.Now()
	gm.replyMu.Lock()
	defer gm.replyMu.Unlock()
	if gm.repliedAt == nil {
		gm.repliedAt = make(map[string]time.Time)
	}
	for id, t := range gm.repliedAt {
		if now.Sub(t) > time.Hour {
			delete(gm.repliedAt, id)
		}
	}
	if _, exists := gm.repliedAt[messageID]; exists {
		return false
	}
	gm.repliedAt[messageID] = now
	return true
}

func (gm *GatewayManager) connectAndListen(ctx context.Context) error {
	gm.setStatus(StatusConnecting, "")

	gwURL, err := gm.client.GetGateway(ctx)
	if err != nil {
		return fmt.Errorf("获取网关失败: %w", err)
	}

	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 15 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, gwURL, nil)
	if err != nil {
		return fmt.Errorf("WebSocket 连接失败: %w", err)
	}
	defer conn.Close()

	token, err := gm.client.GetAccessToken(ctx, false)
	if err != nil {
		return fmt.Errorf("获取鉴权 Token 失败: %w", err)
	}

	var writeMu sync.Mutex
	safeWriteJSON := func(v interface{}) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(v)
	}

	conn.SetPingHandler(func(appData string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	var heartbeatInterval time.Duration = 40 * time.Second
	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)

	// 读取初始 Hello 包 (OpCode 10)
	var p payload
	if err := conn.ReadJSON(&p); err != nil {
		return fmt.Errorf("读取初始 Hello 包失败: %w", err)
	}

	if p.Op != 10 {
		return fmt.Errorf("期望 Hello (Op 10)，收到 Op %d: %s", p.Op, string(p.D))
	}

	var hello helloData
	if err := json.Unmarshal(p.D, &hello); err == nil && hello.HeartbeatInterval > 0 {
		heartbeatInterval = time.Duration(hello.HeartbeatInterval) * time.Millisecond
	}

	// 发送 Identify (OpCode 2)
	// 33554432 (1 << 25) 订阅 C2C 私聊事件
	identifyData := map[string]interface{}{
		"token":   "QQBot " + token,
		"intents": 1 << 25,
		"shard":   []int{0, 1},
		"properties": map[string]string{
			"$os":      runtime.GOOS,
			"$browser": "ncmm-qqbot",
			"$device":  "ncmm-qqbot",
		},
	}
	identifyPayload := map[string]interface{}{
		"op": 2,
		"d":  identifyData,
	}
	if err := safeWriteJSON(identifyPayload); err != nil {
		return fmt.Errorf("发送 Identify 失败: %w", err)
	}

	// 开启心跳协程
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				gm.mu.RLock()
				seq := gm.lastSeq
				gm.mu.RUnlock()

				var hb map[string]interface{}
				if seq > 0 {
					hb = map[string]interface{}{"op": 1, "d": seq}
				} else {
					hb = map[string]interface{}{"op": 1, "d": nil}
				}
				if err := safeWriteJSON(hb); err != nil {
					log.Printf("[QQBot-Gateway] 发送心跳失败: %v", err)
					_ = conn.Close()
					return
				}
			}
		}
	}()

	// 循环接收消息
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		var in payload
		if err := conn.ReadJSON(&in); err != nil {
			return fmt.Errorf("读取消息错误: %w", err)
		}

		if in.S != nil {
			gm.mu.Lock()
			gm.lastSeq = *in.S
			gm.mu.Unlock()
		}

		switch in.Op {
		case 0: // Dispatch 事件下发
			gm.handleDispatch(ctx, in)
		case 7: // Reconnect
			log.Printf("[QQBot-Gateway] 收到重连指令 (OpCode 7)")
			return fmt.Errorf("网关要求重新连接")
		case 9: // Invalid Session
			log.Printf("[QQBot-Gateway] 会话无效 (OpCode 9)")
			return fmt.Errorf("会话无效")
		case 11: // Heartbeat ACK
			// 心跳应答正常
		}
	}
}

func (gm *GatewayManager) handleDispatch(ctx context.Context, in payload) {
	eventType := strings.ToUpper(in.T)

	if eventType == "READY" {
		var ready readyData
		if err := json.Unmarshal(in.D, &ready); err == nil {
			gm.mu.Lock()
			gm.sessionID = ready.SessionID
			gm.mu.Unlock()
			gm.setStatus(StatusConnected, "")
			log.Printf("[QQBot-Gateway] 机器人认证就绪！已连接灵魂: 机器人名称=%s, ID=%s", ready.User.Username, ready.User.ID)

			go func() {
				time.Sleep(1 * time.Second)
				_ = gm.client.SyncCommandPanel(context.Background())
			}()
		}
		return
	}

	if eventType == "C2C_MESSAGE_CREATE" || eventType == "DIRECT_MESSAGE_CREATE" {
		var msg C2CMessage
		if err := json.Unmarshal(in.D, &msg); err != nil {
			log.Printf("[QQBot-Gateway] 解析 %s 消息失败: %v, raw=%s", eventType, err, string(in.D))
			return
		}
		if msg.Author.Bot {
			return
		}
		if !gm.markReply(msg.ID) {
			log.Printf("[QQBot-Gateway] 忽略重复投递消息: %s", msg.ID)
			return
		}
		if gm.handler != nil {
			go gm.handler(ctx, &msg)
		}
		return
	}

	log.Printf("[QQBot-Gateway] 收到事件: %s", eventType)
}
