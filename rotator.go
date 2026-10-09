package main

import (
	"fmt"
	"os"
	"sync"
)

// RotatingFileWriter 线程安全且支持按体积轮转与备份清理的日志写入器
type RotatingFileWriter struct {
	mu          sync.Mutex
	filePath    string
	maxSize     int64 // 单文件最大字节数，如 5MB
	maxBackups  int   // 最多保留历史备份数，如 1
	file        *os.File
	currentSize int64
}

// NewRotatingFileWriter 创建日志轮转器
func NewRotatingFileWriter(filePath string, maxSize int64, maxBackups int) (*RotatingFileWriter, error) {
	w := &RotatingFileWriter{
		filePath:   filePath,
		maxSize:    maxSize,
		maxBackups: maxBackups,
	}

	// 启动自检：若已有日志文件已超标，立即轮转
	if fi, err := os.Stat(filePath); err == nil {
		if fi.Size() >= maxSize {
			_ = w.rotate()
		} else {
			w.currentSize = fi.Size()
		}
	}

	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", filePath, err)
	}
	w.file = file

	return w, nil
}

func (w *RotatingFileWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	writeLen := int64(len(p))
	if w.file != nil && w.currentSize+writeLen > w.maxSize {
		_ = w.rotate()
	}

	if w.file == nil {
		file, err := os.OpenFile(w.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return 0, err
		}
		w.file = file
		w.currentSize = 0
	}

	n, err = w.file.Write(p)
	w.currentSize += int64(n)
	return n, err
}

func (w *RotatingFileWriter) rotate() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}

	if w.maxBackups >= 1 {
		backupPath := fmt.Sprintf("%s.1", w.filePath)
		_ = os.Remove(backupPath)
		_ = os.Rename(w.filePath, backupPath)
	} else {
		_ = os.Remove(w.filePath)
	}

	w.currentSize = 0
	return nil
}

func (w *RotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}
