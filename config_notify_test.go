package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncHostNotify(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ncmm-notify-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	notifyPath := filepath.Join(tempDir, "notify.yaml")

	autoTrue := true
	autoFalse := false

	cfg := &Config{
		AutoNotify: &autoTrue,
		Port:       5606,
		NCMMHome:   tempDir,
	}

	// 1. Test creation when file does not exist
	if err := SyncHostNotify(cfg); err != nil {
		t.Fatalf("SyncHostNotify failed on non-existent file: %v", err)
	}

	data, err := os.ReadFile(notifyPath)
	if err != nil {
		t.Fatalf("Failed to read created notify.yaml: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "enabled: true") || !strings.Contains(content, "http://127.0.0.1:5606/notify") {
		t.Fatalf("Created content mismatch: %s", content)
	}

	// 2. Test updating with existing channels and comments
	existingContent := `# Existing comments
bark:
  enabled: true
  key: "test-bark-key"

# Original webhook
webhook:
  enabled: false
  url: ""
  method: POST
`
	if err := os.WriteFile(notifyPath, []byte(existingContent), 0644); err != nil {
		t.Fatalf("Failed to write mock notify.yaml: %v", err)
	}

	// Change port to 5608 and test sync
	cfg.Port = 5608
	if err := SyncHostNotify(cfg); err != nil {
		t.Fatalf("SyncHostNotify failed on existing file: %v", err)
	}

	data, err = os.ReadFile(notifyPath)
	if err != nil {
		t.Fatalf("Failed to read updated notify.yaml: %v", err)
	}
	content = string(data)

	// Verify bark is preserved
	if !strings.Contains(content, "test-bark-key") {
		t.Fatalf("Existing bark configuration was lost! Content: %s", content)
	}
	// Verify webhook is updated with new port
	if !strings.Contains(content, "enabled: true") || !strings.Contains(content, "http://127.0.0.1:5608/notify") {
		t.Fatalf("Webhook was not properly updated! Content: %s", content)
	}

	// 3. Test disabling auto_notify
	cfg.AutoNotify = &autoFalse
	if err := SyncHostNotify(cfg); err != nil {
		t.Fatalf("SyncHostNotify failed when disabling: %v", err)
	}

	data, err = os.ReadFile(notifyPath)
	if err != nil {
		t.Fatalf("Failed to read disabled notify.yaml: %v", err)
	}
	content = string(data)
	if !strings.Contains(content, "enabled: false") {
		t.Fatalf("Webhook should be disabled! Content: %s", content)
	}
	if !strings.Contains(content, "test-bark-key") {
		t.Fatalf("Existing bark configuration was lost when disabled! Content: %s", content)
	}
}
