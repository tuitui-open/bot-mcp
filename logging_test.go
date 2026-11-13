package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDailyLogCreationAndAppend(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "log")
	now := func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }
	for _, message := range []string{"first", "second"} {
		w, err := newDailyLogWriter(directory, 7, now)
		if err != nil {
			t.Fatal(err)
		}
		slog.New(slog.NewJSONHandler(w, nil)).Info(message)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(filepath.Join(directory, "2026-11-13.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"msg":"first"`) || !strings.Contains(string(content), `"msg":"second"`) {
		t.Fatal("同一天重启应追加到同一个日志文件")
	}
	if strings.Count(string(content), "\n\n") != 2 {
		t.Fatal("每条日志之间应有一个空行")
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 {
		t.Fatal("每天应只有一个日志文件")
	}
}

func TestDailyLogRotationAndRetention(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"2026-11-06.log", "2026-11-07.log", "2026-11-12.log", "notes.txt", "custom.log"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var seconds atomic.Int64
	seconds.Store(time.Date(2026, 11, 13, 23, 59, 59, 0, time.UTC).Unix())
	w, err := newDailyLogWriter(directory, 7, func() time.Time { return time.Unix(seconds.Load(), 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("before midnight\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "2026-11-06.log")); !os.IsNotExist(err) {
		t.Fatal("启动时未清理过期日志")
	}
	if _, err := os.Stat(filepath.Join(directory, "2026-11-07.log")); err != nil {
		t.Fatal("应保留今天及之前六天")
	}
	seconds.Add(2)
	if _, err := w.Write([]byte("after midnight\n")); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"2026-11-13.log": "before midnight\n\n", "2026-11-14.log": "after midnight\n\n"} {
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || string(content) != want {
			t.Fatal("跨天未正确切换日志文件")
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "2026-11-07.log")); !os.IsNotExist(err) {
		t.Fatal("跨天未清理过期日志")
	}
	for _, name := range []string{"2026-11-12.log", "notes.txt", "custom.log"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatal("不应删除近期日志或其他文件")
		}
	}
}

func TestDailyLogConcurrentWrites(t *testing.T) {
	directory := t.TempDir()
	now := func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }
	w, err := newDailyLogWriter(directory, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := fmt.Fprintf(w, "entry-%d\n", i); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "2026-11-13.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(strings.TrimSpace(string(content)), "\n\n")) != 50 {
		t.Fatal("并发写入丢失日志")
	}
	if _, err := w.Write([]byte("closed")); err == nil {
		t.Fatal("关闭后不应继续写入")
	}
}
