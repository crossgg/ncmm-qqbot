package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	TokenURL   = "https://api.bot.qq.com/app/getAppAccessToken"
	BaseAPIURL = "https://api.bot.qq.com"
)

type Client struct {
	appID        string
	clientSecret string
	httpClient   *http.Client

	mu          sync.RWMutex
	accessToken string
	expireAt    time.Time

	seqMu   sync.Mutex
	msgSeqs map[string]int
}

func NewClient(appID, clientSecret string) *Client {
	return &Client{
		appID:        strings.TrimSpace(appID),
		clientSecret: strings.TrimSpace(clientSecret),
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		msgSeqs:      make(map[string]int),
	}
}

func (c *Client) nextMsgSeq(msgID string) int {
	if msgID == "" {
		return 0
	}
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	if c.msgSeqs == nil {
		c.msgSeqs = make(map[string]int)
	}
	c.msgSeqs[msgID]++
	seq := c.msgSeqs[msgID]
	if len(c.msgSeqs) > 2000 {
		c.msgSeqs = make(map[string]int)
		c.msgSeqs[msgID] = seq
	}
	return seq
}

func (c *Client) UpdateCredentials(appID, clientSecret string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.appID = strings.TrimSpace(appID)
	c.clientSecret = strings.TrimSpace(clientSecret)
	c.accessToken = ""
	c.expireAt = time.Time{}
}

type tokenResponse struct {
	AccessToken string      `json:"access_token"`
	ExpiresIn   interface{} `json:"expires_in"` // 可以是 string 或 int
	Code        int         `json:"code"`
	Message     string      `json:"message"`
}

func (c *Client) GetAccessToken(ctx context.Context, forceRefresh bool) (string, error) {
	c.mu.RLock()
	if !forceRefresh && c.accessToken != "" && time.Now().Before(c.expireAt.Add(-5*time.Minute)) {
		token := c.accessToken
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double check
	if !forceRefresh && c.accessToken != "" && time.Now().Before(c.expireAt.Add(-5*time.Minute)) {
		return c.accessToken, nil
	}

	if c.appID == "" || c.clientSecret == "" {
		return "", fmt.Errorf("QQ 机器人 AppID 或 ClientSecret 未配置")
	}

	payload := map[string]string{
		"appId":        c.appID,
		"clientSecret": c.clientSecret,
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 AccessToken 失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("获取 AccessToken 返回状态码 %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var res tokenResponse
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return "", fmt.Errorf("解析 AccessToken 响应失败: %w", err)
	}

	if res.AccessToken == "" {
		return "", fmt.Errorf("未返回 AccessToken (code=%d, msg=%s)", res.Code, res.Message)
	}

	var expiresInSec int64 = 7200
	switch v := res.ExpiresIn.(type) {
	case float64:
		expiresInSec = int64(v)
	case string:
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			expiresInSec = sec
		}
	}

	c.accessToken = res.AccessToken
	c.expireAt = time.Now().Add(time.Duration(expiresInSec) * time.Second)
	return c.accessToken, nil
}

type GatewayResponse struct {
	URL string `json:"url"`
}

func (c *Client) GetGateway(ctx context.Context) (string, error) {
	token, err := c.GetAccessToken(ctx, false)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", BaseAPIURL+"/gateway", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "QQBot "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 Gateway 失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("获取 Gateway 返回状态码 %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var res GatewayResponse
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return "", fmt.Errorf("解析 Gateway 响应失败: %w", err)
	}
	if res.URL == "" {
		return "", fmt.Errorf("返回 Gateway 地址为空: %s", string(bodyBytes))
	}

	return res.URL, nil
}

type SendMessageReq struct {
	Content string `json:"content"`
	MsgType int    `json:"msg_type"` // 0: 文本, 2: markdown
	MsgID   string `json:"msg_id,omitempty"`
	MsgSeq  int    `json:"msg_seq,omitempty"`
}

type SendMessageResp struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Code      int    `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
}

// SendC2CMessage 发送私聊消息给指定用户
func (c *Client) SendC2CMessage(ctx context.Context, openID, content, msgID string) (*SendMessageResp, error) {
	token, err := c.GetAccessToken(ctx, false)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("%s/v2/users/%s/messages", BaseAPIURL, openID)
	reqData := map[string]interface{}{
		"content":  content,
		"msg_type": 0,
	}
	if msgID != "" {
		reqData["msg_id"] = msgID
		reqData["msg_seq"] = c.nextMsgSeq(msgID)
	}
	data, _ := json.Marshal(reqData)

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送 C2C 消息失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("发送 C2C 消息返回状态码 %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var res SendMessageResp
	_ = json.Unmarshal(bodyBytes, &res)
	return &res, nil
}

// SendC2CImage 发送图片给指定私聊用户（支持 multipart 上传富媒体并由服务端代发）
func (c *Client) SendC2CImage(ctx context.Context, openID string, imageBytes []byte, msgID string) error {
	token, err := c.GetAccessToken(ctx, false)
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("%s/v2/users/%s/files", BaseAPIURL, openID)

	var b bytes.Buffer
	w := multipart.NewWriter(&b)

	// file_type: 1 为图片
	_ = w.WriteField("file_type", "1")
	// srv_send_msg: true 让官方服务器直接将上传的富媒体发送到聊天窗口
	_ = w.WriteField("srv_send_msg", "true")
	if msgID != "" {
		_ = w.WriteField("msg_id", msgID)
		_ = w.WriteField("msg_seq", strconv.Itoa(c.nextMsgSeq(msgID)))
	}

	part, err := w.CreateFormFile("file_data", "qrcode.png")
	if err != nil {
		return fmt.Errorf("创建表单文件失败: %w", err)
	}
	if _, err := part.Write(imageBytes); err != nil {
		return fmt.Errorf("写入图片数据失败: %w", err)
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, &b)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("上传富媒体失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("发送图片返回状态码 %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// TestCredentials 测试凭证是否有效
func (c *Client) TestCredentials(ctx context.Context) (string, error) {
	token, err := c.GetAccessToken(ctx, true)
	if err != nil {
		return "", err
	}
	gateway, err := c.GetGateway(ctx)
	if err != nil {
		return "", fmt.Errorf("获取网关成功但连接校验失败: %w", err)
	}
	_ = token
	return gateway, nil
}
