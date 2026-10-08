package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type Config struct {
	AppID        string `yaml:"app_id" json:"app_id"`
	ClientSecret string `yaml:"client_secret" json:"client_secret"`
	AdminOpenID  string `yaml:"admin_openid" json:"admin_openid"`
	Port         int    `yaml:"port" json:"port"`
	Host         string `yaml:"host" json:"host"`
	NCMMHome     string `yaml:"ncmm_home" json:"ncmm_home"`
	NCMMExe      string `yaml:"ncmm_exe" json:"ncmm_exe"`
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

	cfg := &Config{
		Port: 5606,
		Host: "127.0.0.1",
	}

	// 自动探测宿主目录与可执行文件路径
	detectHostEnv(cfg)

	if _, err := os.Stat(cm.path); os.IsNotExist(err) {
		cm.cfg = cfg
		_ = cm.saveLocked(cfg)
		return cfg, nil
	}

	data, err := os.ReadFile(cm.path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
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
	return cfg, nil
}

func (cm *ConfigManager) Save(newCfg *Config) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.saveLocked(newCfg)
}

func (cm *ConfigManager) saveLocked(newCfg *Config) error {
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
