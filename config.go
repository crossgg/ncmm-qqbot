package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type Config struct {
	AutoNotify   *bool  `yaml:"auto_notify" json:"auto_notify"`
	AppID        string `yaml:"app_id" json:"app_id"`
	ClientSecret string `yaml:"client_secret" json:"client_secret"`
	AdminOpenID  string `yaml:"admin_openid" json:"admin_openid"`
	Port         int    `yaml:"port" json:"port"`
	Host         string `yaml:"host" json:"host"`
	NCMMHome     string `yaml:"ncmm_home" json:"ncmm_home"`
	NCMMExe      string `yaml:"ncmm_exe" json:"ncmm_exe"`
}

func (c *Config) IsAutoNotify() bool {
	if c.AutoNotify == nil {
		return true
	}
	return *c.AutoNotify
}

type ConfigManager struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
}

func NewConfigManager(path string) *ConfigManager {
	return &ConfigManager{path: path}
}

func (cm *ConfigManager) Get() Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if cm.cfg == nil {
		return Config{}
	}
	return *cm.cfg
}

func (cm *ConfigManager) Load() (*Config, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	defaultAutoNotify := true
	cfg := &Config{
		AutoNotify: &defaultAutoNotify,
		Port:       5606,
		Host:       "127.0.0.1",
	}

	// 自动探测宿主目录与可执行文件路径
	detectHostEnv(cfg)

	if _, err := os.Stat(cm.path); os.IsNotExist(err) {
		cm.cfg = cfg
		_ = cm.saveLocked(cfg)
		_ = SyncHostNotify(cfg)
		return cfg, nil
	}

	data, err := os.ReadFile(cm.path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	if cfg.AutoNotify == nil {
		cfg.AutoNotify = &defaultAutoNotify
	}
	if cfg.Port <= 0 {
		cfg.Port = 5606
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.NCMMHome == "" || cfg.NCMMExe == "" {
		detectHostEnv(cfg)
	}

	cm.cfg = cfg
	_ = SyncHostNotify(cfg)
	return cfg, nil
}

func (cm *ConfigManager) Save(newCfg *Config) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if err := cm.saveLocked(newCfg); err != nil {
		return err
	}
	_ = SyncHostNotify(newCfg)
	return nil
}

func (cm *ConfigManager) saveLocked(newCfg *Config) error {
	if newCfg.AutoNotify == nil {
		defaultAutoNotify := true
		newCfg.AutoNotify = &defaultAutoNotify
	}
	dir := filepath.Dir(cm.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	data, err := yaml.Marshal(newCfg)
	if err != nil {
		return fmt.Errorf("格式化配置失败: %w", err)
	}

	tmpFile := cm.path + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("写入临时配置失败: %w", err)
	}

	if err := os.Rename(tmpFile, cm.path); err != nil {
		_ = os.Remove(tmpFile)
		return os.WriteFile(cm.path, data, 0644)
	}

	cm.cfg = newCfg
	return nil
}

// detectHostEnv 尝试自动探测 ncmm 主目录与可执行文件
func detectHostEnv(cfg *Config) {
	// 1. 优先读取环境变量
	if envHome := os.Getenv("NCMM_HOME"); envHome != "" && cfg.NCMMHome == "" {
		cfg.NCMMHome = filepath.Clean(envHome)
	}

	// 2. 向上逐级探测（插件位于 plugins/ncmm-qqbot 下）
	if cfg.NCMMHome == "" {
		if exe, err := os.Executable(); err == nil {
			curDir := filepath.Dir(exe)
			// 如果在 plugins/ncmm-qqbot 目录下，祖父目录即为主目录
			candidate := filepath.Clean(filepath.Join(curDir, "..", ".."))
			if isNCMMDir(candidate) {
				cfg.NCMMHome = candidate
			} else {
				// 尝试父级
				candidate = filepath.Clean(filepath.Join(curDir, ".."))
				if isNCMMDir(candidate) {
					cfg.NCMMHome = candidate
				}
			}
		}
	}

	// 3. 兜底为工作目录上一级
	if cfg.NCMMHome == "" {
		if wd, err := os.Getwd(); err == nil {
			candidate := filepath.Clean(filepath.Join(wd, "..", ".."))
			if isNCMMDir(candidate) {
				cfg.NCMMHome = candidate
			} else {
				candidate = filepath.Clean(filepath.Join(wd, ".."))
				if isNCMMDir(candidate) {
					cfg.NCMMHome = candidate
				}
			}
		}
	}

	// 探测 ncmm.exe
	if cfg.NCMMExe == "" && cfg.NCMMHome != "" {
		exeName := "ncmm"
		if isWindows() {
			exeName = "ncmm.exe"
		}
		candidate := filepath.Join(cfg.NCMMHome, exeName)
		if fileExists(candidate) {
			cfg.NCMMExe = candidate
		}
	}
}

func isNCMMDir(dir string) bool {
	// 如果包含 config.yaml 或 ncmm.exe 或 api/ 说明是根目录
	return fileExists(filepath.Join(dir, "config.yaml")) ||
		fileExists(filepath.Join(dir, "config", "config.yaml")) ||
		fileExists(filepath.Join(dir, "ncmm.exe")) ||
		fileExists(filepath.Join(dir, "ncmm"))
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func isWindows() bool {
	return strings.ToLower(os.Getenv("OS")) == "windows_nt" || filepath.Separator == '\\'
}

// SyncHostNotify 同步更新宿主 notify.yaml 的 webhook 配置
func SyncHostNotify(cfg *Config) error {
	if cfg == nil || cfg.NCMMHome == "" {
		return nil
	}

	// 1. 定位宿主 notify.yaml 路径（优先根目录，其次 config/notify.yaml）
	targetFile := filepath.Join(cfg.NCMMHome, "notify.yaml")
	configSubFile := filepath.Join(cfg.NCMMHome, "config", "notify.yaml")
	if !fileExists(targetFile) && fileExists(configSubFile) {
		targetFile = configSubFile
	}

	enabled := cfg.IsAutoNotify()
	port := cfg.Port
	if port <= 0 {
		port = 5606
	}
	webhookURL := fmt.Sprintf("http://127.0.0.1:%d/notify", port)

	// 2. 如果文件不存在
	if !fileExists(targetFile) {
		if !enabled {
			return nil
		}
		initialContent := fmt.Sprintf(`# ncmm 通知通道配置
# 由 ncmm-qqbot 自动生成并管理本地 Webhook 通道

webhook:
  enabled: true
  url: "%s"
  method: POST
`, webhookURL)
		dir := filepath.Dir(targetFile)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建通知配置目录失败: %w", err)
		}
		if err := os.WriteFile(targetFile, []byte(initialContent), 0644); err != nil {
			return fmt.Errorf("写入通知配置文件失败: %w", err)
		}
		log.Printf("[QQBot-Notify] 已自动创建并启用宿主通知配置: %s (URL: %s)", targetFile, webhookURL)
		return nil
	}

	// 3. 文件已存在，基于 yaml.Node 无损增量修改（保留已有注释和其他通道配置）
	data, err := os.ReadFile(targetFile)
	if err != nil {
		return fmt.Errorf("读取宿主通知文件失败: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("解析宿主通知文件失败: %w", err)
	}

	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if len(doc.Content) == 0 {
		doc.Content = append(doc.Content, &yaml.Node{
			Kind: yaml.MappingNode,
			Tag:  "!!map",
		})
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("宿主通知文件根节点不是 Map 格式")
	}

	var webhookNode *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "webhook" {
			webhookNode = root.Content[i+1]
			break
		}
	}

	if webhookNode == nil {
		if !enabled {
			return nil
		}
		keyNode := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: "webhook",
		}
		valNode := &yaml.Node{
			Kind: yaml.MappingNode,
			Tag:  "!!map",
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enabled"},
				{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "url"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: webhookURL},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "method"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "POST"},
			},
		}
		root.Content = append(root.Content, keyNode, valNode)
	} else {
		if webhookNode.Kind != yaml.MappingNode {
			return fmt.Errorf("webhook 节点格式异常")
		}

		var curURL string
		for i := 0; i < len(webhookNode.Content); i += 2 {
			if webhookNode.Content[i].Value == "url" {
				curURL = webhookNode.Content[i+1].Value
				break
			}
		}

		isOurBot := strings.Contains(curURL, "127.0.0.1") ||
			strings.Contains(curURL, "localhost") ||
			curURL == "" ||
			strings.HasSuffix(curURL, "/notify")

		if !enabled {
			if isOurBot {
				setOrUpdateMapField(webhookNode, "enabled", "false", "!!bool")
			}
		} else {
			setOrUpdateMapField(webhookNode, "enabled", "true", "!!bool")
			setOrUpdateMapField(webhookNode, "url", webhookURL, "!!str")
			if getMapFieldValue(webhookNode, "method") == "" {
				setOrUpdateMapField(webhookNode, "method", "POST", "!!str")
			}
		}
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("格式化通知配置失败: %w", err)
	}

	if err := os.WriteFile(targetFile, out, 0644); err != nil {
		return fmt.Errorf("保存宿主通知文件失败: %w", err)
	}

	log.Printf("[QQBot-Notify] 已自动同步宿主通知配置: %s (enabled: %v, url: %s)", targetFile, enabled, webhookURL)
	return nil
}

func getMapFieldValue(m *yaml.Node, key string) string {
	for i := 0; i < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1].Value
		}
	}
	return ""
}

func setOrUpdateMapField(m *yaml.Node, key, val, tag string) {
	for i := 0; i < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1].Value = val
			m.Content[i+1].Tag = tag
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: val},
	)
}

