package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	logDirName  = "logs"
	logFileName = "server.log"
	maxLogSize  = 10 << 20
	maxLogFiles = 5
)

// newLogger 返回同时写入 stderr 与滚动文件的 logger。
func newLogger(stderr io.Writer, level slog.Level, dataDir string) (*slog.Logger, io.Closer, error) {
	if dataDir == "" {
		out := stderr
		if out == nil {
			out = io.Discard
		}
		return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})), nopCloser{}, nil
	}
	w, err := newRotatingWriter(filepath.Join(dataDir, logDirName), logFileName, maxLogSize, maxLogFiles-1)
	if err != nil {
		return nil, nil, err
	}

	var out io.Writer = w
	if stderr != nil {
		out = io.MultiWriter(stderr, w)
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})), w, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

type rotatingWriter struct {
	mu         sync.Mutex
	dir        string
	path       string
	baseName   string
	maxSize    int64
	maxBackups int
	file       *os.File
	size       int64
}

func newRotatingWriter(dir, fileName string, maxSize int64, maxBackups int) (*rotatingWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	w := &rotatingWriter{
		dir:        dir,
		path:       filepath.Join(dir, fileName),
		baseName:   strings.TrimSuffix(fileName, filepath.Ext(fileName)),
		maxSize:    maxSize,
		maxBackups: maxBackups,
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("读取日志文件状态失败: %w", err)
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("关闭日志文件失败: %w", err)
	}
	w.file = nil

	backup := filepath.Join(
		w.dir,
		fmt.Sprintf("%s-%s.log", w.baseName, time.Now().Format("20060102-150405.000000000")),
	)
	if err := os.Rename(w.path, backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("滚动日志失败: %w", err)
	}
	if err := w.open(); err != nil {
		return err
	}
	w.cleanup()
	return nil
}

func (w *rotatingWriter) cleanup() {
	matches, err := filepath.Glob(filepath.Join(w.dir, w.baseName+"-*.log"))
	if err != nil {
		return
	}
	sort.Strings(matches)
	for len(matches) > w.maxBackups {
		_ = os.Remove(matches[0])
		matches = matches[1:]
	}
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
