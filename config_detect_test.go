package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectHostEnv_NCMM_EXE(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ncmm-detect-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	fakeExe := filepath.Join(tempDir, "fake_ncmm.exe")
	if err := os.WriteFile(fakeExe, []byte("fake"), 0755); err != nil {
		t.Fatalf("failed to create fake exe: %v", err)
	}

	origEnv := os.Getenv("NCMM_EXE")
	os.Setenv("NCMM_EXE", fakeExe)
	defer os.Setenv("NCMM_EXE", origEnv)

	cfg := &Config{}
	detectHostEnv(cfg)

	if cfg.NCMMExe != fakeExe {
		t.Errorf("detectHostEnv() NCMMExe = %q; want %q", cfg.NCMMExe, fakeExe)
	}
}

func TestDetectHostEnv_NCMM_HOME(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ncmm-detect-home-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	origEnvExe := os.Getenv("NCMM_EXE")
	os.Unsetenv("NCMM_EXE")
	defer func() {
		if origEnvExe != "" {
			os.Setenv("NCMM_EXE", origEnvExe)
		}
	}()

	exeName := "ncmm"
	if isWindows() {
		exeName = "ncmm.exe"
	}

	fakeExe := filepath.Join(tempDir, exeName)
	if err := os.WriteFile(fakeExe, []byte("fake"), 0755); err != nil {
		t.Fatalf("failed to create fake exe: %v", err)
	}

	cfg := &Config{
		NCMMHome: tempDir,
	}
	detectHostEnv(cfg)

	if cfg.NCMMExe != fakeExe {
		t.Errorf("detectHostEnv() NCMMExe = %q; want %q", cfg.NCMMExe, fakeExe)
	}
}
