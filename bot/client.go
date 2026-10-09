package bot

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// SendC2CImage 通过分片预签名上传模式发送富媒体图片给指定私聊用户
func (c *Client) SendC2CImage(ctx context.Context, openID string, imageBytes []byte, msgID string) error {
	token, err := c.GetAccessToken(ctx, false)
	if err != nil {
		return err
	}

	fileSize := len(imageBytes)
	if fileSize == 0 {
		return fmt.Errorf("图片数据为空")
	}

	md5Hash := md5.Sum(imageBytes)
	md5Hex := hex.EncodeToString(md5Hash[:])

	sha1Hash := sha1.Sum(imageBytes)
	sha1Hex := hex.EncodeToString(sha1Hash[:])

	// 1. Upload Prepare
	prepareURL := fmt.Sprintf("%s/v2/users/%s/upload_prepare", BaseAPIURL, openID)
	prepareReqBody := map[string]interface{}{
		"file_type": 1, // 1: 图片
		"file_size": strconv.Itoa(fileSize),
		"file_name": "qrcode.png",
		"md5":       md5Hex,
		"sha1":      sha1Hex,
		"md5_10m":   md5Hex,
	}
	prepJSON, _ := json.Marshal(prepareReqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", prepareURL, bytes.NewReader(prepJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求 upload_prepare 失败: %w", err)
	}
	defer resp.Body.Close()

	prepRespBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("upload_prepare 返回状态码 %d: %s", resp.StatusCode, string(prepRespBytes))
	}

	var prepRes struct {
		UploadID string `json:"upload_id"`
		Parts    []struct {
			Index        int    `json:"index"`
			PresignedURL string `json:"presigned_url"`
			BlockSize    string `json:"block_size"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(prepRespBytes, &prepRes); err != nil {
		return fmt.Errorf("解析 upload_prepare 响应失败: %w", err)
	}
	if prepRes.UploadID == "" || len(prepRes.Parts) == 0 {
		return fmt.Errorf("upload_prepare 返回数据不完整: %s", string(prepRespBytes))
	}

	// 2. PUT to presigned_url (COS 预签名 URL 不带 QQBot Authorization Header)
	putReq, err := http.NewRequestWithContext(ctx, "PUT", prepRes.Parts[0].PresignedURL, bytes.NewReader(imageBytes))
	if err != nil {
		return fmt.Errorf("创建 PUT 请求失败: %w", err)
	}
	putReq.Header.Set("Content-Type", "image/png")
	putReq.Header.Set("Content-Length", strconv.Itoa(fileSize))

	putResp, err := c.httpClient.Do(putReq)
	if err != nil {
		return fmt.Errorf("PUT 上传分片数据失败: %w", err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusCreated && putResp.StatusCode != http.StatusNoContent {
		putRespBytes, _ := io.ReadAll(putResp.Body)
		return fmt.Errorf("PUT 上传分片返回状态码 %d: %s", putResp.StatusCode, string(putRespBytes))
	}

	// 3. Upload Part Finish
	finishURL := fmt.Sprintf("%s/v2/users/%s/upload_part_finish", BaseAPIURL, openID)
	finishReqBody := map[string]interface{}{
		"upload_id":  prepRes.UploadID,
		"part_index": 0,
		"block_size": strconv.Itoa(fileSize),
		"md5":        md5Hex,
	}
	finJSON, _ := json.Marshal(finishReqBody)
	finReq, err := http.NewRequestWithContext(ctx, "POST", finishURL, bytes.NewReader(finJSON))
	if err != nil {
		return err
	}
	finReq.Header.Set("Authorization", "QQBot "+token)
	finReq.Header.Set("Content-Type", "application/json")

	finResp, err := c.httpClient.Do(finReq)
	if err != nil {
		return fmt.Errorf("请求 upload_part_finish 失败: %w", err)
	}
	defer finResp.Body.Close()
	if finResp.StatusCode != http.StatusOK && finResp.StatusCode != http.StatusNoContent && finResp.StatusCode != http.StatusCreated {
		finRespBytes, _ := io.ReadAll(finResp.Body)
		return fmt.Errorf("upload_part_finish 返回状态码 %d: %s", finResp.StatusCode, string(finRespBytes))
	}

	// 4. Files 合并获取 file_info
	filesURL := fmt.Sprintf("%s/v2/users/%s/files", BaseAPIURL, openID)
	filesReqBody := map[string]interface{}{
		"file_type":    1,
		"srv_send_msg": false,
		"file_name":    "qrcode.png",
		"upload_id":    prepRes.UploadID,
	}
	filesJSON, _ := json.Marshal(filesReqBody)
	filesReq, err := http.NewRequestWithContext(ctx, "POST", filesURL, bytes.NewReader(filesJSON))
	if err != nil {
		return err
	}
	filesReq.Header.Set("Authorization", "QQBot "+token)
	filesReq.Header.Set("Content-Type", "application/json")

	filesResp, err := c.httpClient.Do(filesReq)
	if err != nil {
		return fmt.Errorf("请求 files 合并失败: %w", err)
	}
	defer filesResp.Body.Close()
	filesRespBytes, _ := io.ReadAll(filesResp.Body)
	if filesResp.StatusCode != http.StatusOK && filesResp.StatusCode != http.StatusCreated {
		return fmt.Errorf("files 合并返回状态码 %d: %s", filesResp.StatusCode, string(filesRespBytes))
	}

	var filesRes struct {
		FileInfo string `json:"file_info"`
	}
	if err := json.Unmarshal(filesRespBytes, &filesRes); err != nil {
		return fmt.Errorf("解析 files 响应失败: %w", err)
	}
	if filesRes.FileInfo == "" {
		return fmt.Errorf("files 返回 file_info 为空: %s", string(filesRespBytes))
	}

	// 5. 调用发送单聊富媒体消息接口 (msg_type: 7)
	msgURL := fmt.Sprintf("%s/v2/users/%s/messages", BaseAPIURL, openID)
	msgReqData := map[string]interface{}{
		"msg_type": 7,
		"media": map[string]string{
			"file_info": filesRes.FileInfo,
		},
	}
	if msgID != "" {
		msgReqData["msg_id"] = msgID
		msgReqData["msg_seq"] = c.nextMsgSeq(msgID)
	}
	msgJSON, _ := json.Marshal(msgReqData)
	msgReq, err := http.NewRequestWithContext(ctx, "POST", msgURL, bytes.NewReader(msgJSON))
	if err != nil {
		return err
	}
	msgReq.Header.Set("Authorization", "QQBot "+token)
	msgReq.Header.Set("Content-Type", "application/json")

	msgResp, err := c.httpClient.Do(msgReq)
	if err != nil {
		return fmt.Errorf("发送富媒体消息失败: %w", err)
	}
	defer msgResp.Body.Close()
	msgRespBytes, _ := io.ReadAll(msgResp.Body)
	if msgResp.StatusCode != http.StatusOK && msgResp.StatusCode != http.StatusCreated {
		return fmt.Errorf("发送富媒体消息返回状态码 %d: %s", msgResp.StatusCode, string(msgRespBytes))
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

type PanelItem struct {
	Type      string `json:"type"` // command 或 link
	Name      string `json:"name"` // 最多 14 字符（约 7 个汉字）
	Desc      string `json:"desc"` // 最多 30 字符（约 15 个汉字）
	OnlyAdmin bool   `json:"only_admin,omitempty"`
	Link      string `json:"link,omitempty"`
}

type PanelConfig struct {
	Items  []PanelItem `json:"items"`
	Remark string      `json:"remark,omitempty"`
}

type PanelRecord struct {
	PanelID    string      `json:"panel_id"`
	Scope      string      `json:"scope"`
	TargetType string      `json:"target_type"`
	Panel      PanelConfig `json:"panel"`
}

type PanelListResponse struct {
	Records []PanelRecord `json:"records"`
}

type CreatePanelRequest struct {
	Scope      string      `json:"scope"`
	TargetType string      `json:"target_type"`
	Panel      PanelConfig `json:"panel"`
}

type UpdatePanelRequest struct {
	Panel PanelConfig `json:"panel"`
}

// SyncCommandPanel 自动注册或更新 QQ 机器人的全局指令面板（/help, /login, /cookie, /status 等）
func (c *Client) SyncCommandPanel(ctx context.Context) error {
	token, err := c.GetAccessToken(ctx, false)
	if err != nil {
		return err
	}

	items := []PanelItem{
		{Type: "command", Name: "/help", Desc: "查看使用帮助及指令列表"},
		{Type: "command", Name: "/login", Desc: "扫码登录并同步网易云凭据"},
		{Type: "command", Name: "/cookie", Desc: "更新或绑定网易云 Cookie"},
		{Type: "command", Name: "/status", Desc: "查看服务与账号状态"},
		{Type: "command", Name: "/bind", Desc: "绑定当前管理员权限"},
		{Type: "command", Name: "/myid", Desc: "查看当前用户的 QQ OpenID"},
		{Type: "command", Name: "/run sign", Desc: "立即触发日常签到任务"},
		{Type: "command", Name: "/run task", Desc: "立即触发全量批量任务"},
	}

	scopes := []string{"c2c", "group"}
	for _, scope := range scopes {
		getURL := fmt.Sprintf("%s/v2/panels?scope=%s&limit=20", BaseAPIURL, scope)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "QQBot "+token)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			log.Printf("[QQBot-Panel] 查询 %s 指令面板失败: %v", scope, err)
			continue
		}

		var listResp PanelListResponse
		_ = json.NewDecoder(resp.Body).Decode(&listResp)
		resp.Body.Close()

		var matchedPanelID string
		for _, rec := range listResp.Records {
			if rec.TargetType == "all" && (rec.Panel.Remark == "ncmm_command_panel" || rec.Scope == scope) {
				matchedPanelID = rec.PanelID
				break
			}
		}

		panelCfg := PanelConfig{
			Items:  items,
			Remark: "ncmm_command_panel",
		}

		if matchedPanelID != "" {
			putURL := fmt.Sprintf("%s/v2/panels/%s", BaseAPIURL, matchedPanelID)
			bodyBytes, _ := json.Marshal(UpdatePanelRequest{Panel: panelCfg})
			putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewReader(bodyBytes))
			if err != nil {
				continue
			}
			putReq.Header.Set("Authorization", "QQBot "+token)
			putReq.Header.Set("Content-Type", "application/json")
			putResp, err := c.httpClient.Do(putReq)
			if err == nil {
				respBody, _ := io.ReadAll(putResp.Body)
				putResp.Body.Close()
				if putResp.StatusCode == http.StatusOK {
					log.Printf("[QQBot-Panel] 成功更新 %s 指令面板 (id=%s)", scope, matchedPanelID)
				} else {
					log.Printf("[QQBot-Panel] 更新 %s 指令面板返回状态码 %d: %s", scope, putResp.StatusCode, string(respBody))
				}
			}
		} else {
			postURL := fmt.Sprintf("%s/v2/panels", BaseAPIURL)
			bodyBytes, _ := json.Marshal(CreatePanelRequest{
				Scope:      scope,
				TargetType: "all",
				Panel:      panelCfg,
			})
			postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, postURL, bytes.NewReader(bodyBytes))
			if err != nil {
				continue
			}
			postReq.Header.Set("Authorization", "QQBot "+token)
			postReq.Header.Set("Content-Type", "application/json")
			postResp, err := c.httpClient.Do(postReq)
			if err == nil {
				respBody, _ := io.ReadAll(postResp.Body)
				postResp.Body.Close()
				if postResp.StatusCode == http.StatusOK || postResp.StatusCode == http.StatusCreated {
					log.Printf("[QQBot-Panel] 成功创建 %s 指令面板: %s", scope, string(respBody))
				} else {
					log.Printf("[QQBot-Panel] 创建 %s 指令面板返回状态码 %d: %s", scope, postResp.StatusCode, string(respBody))
				}
			}
		}
	}

	return nil
}
