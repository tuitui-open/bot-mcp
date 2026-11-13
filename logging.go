package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type dailyLogWriter struct {
	mu        sync.Mutex
	directory string
	keepDays  int
	now       func() time.Time
	file      *os.File
	date      string
	closed    bool
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newDailyLogWriter(directory string, keepDays int, now func() time.Time) (*dailyLogWriter, error) {
	if err := os.MkdirAll(directory, 0750); err != nil {
		return nil, err
	}
	w := &dailyLogWriter{directory: directory, keepDays: keepDays, now: now, stop: make(chan struct{}), done: make(chan struct{})}
	if err := w.rotate(now()); err != nil {
		return nil, err
	}
	go w.maintain()
	return w, nil
}

// rotate is called with mu held once the writer is running.
func (w *dailyLogWriter) rotate(now time.Time) error {
	date := now.Format("2006-01-02")
	if w.date == date {
		return nil
	}
	if err := w.cleanup(now); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(w.directory, date+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	if w.file != nil {
		w.file.Close()
	}
	w.file, w.date = file, date
	return nil
}

func (w *dailyLogWriter) cleanup(now time.Time) error {
	entries, err := os.ReadDir(w.directory)
	if err != nil {
		return err
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	cutoff := today.AddDate(0, 0, -(w.keepDays - 1))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", strings.TrimSuffix(entry.Name(), ".log"), now.Location())
		if err != nil || !date.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(w.directory, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (w *dailyLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if err := w.rotate(w.now()); err != nil {
		return 0, err
	}
	n, err := w.file.Write(p)
	if err == nil && n > 0 {
		_, err = w.file.Write([]byte("\n"))
	}
	return n, err
}

func (w *dailyLogWriter) maintain() {
	defer close(w.done)
	for {
		now := w.now()
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-w.stop:
			timer.Stop()
			return
		case <-timer.C:
			w.mu.Lock()
			// A subsequent write retries rotation if maintenance fails.
			w.rotate(w.now())
			w.mu.Unlock()
		}
	}
}

func (w *dailyLogWriter) Close() error {
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
		w.mu.Lock()
		defer w.mu.Unlock()
		w.closed = true
		w.closeErr = w.file.Close()
	})
	return w.closeErr
}

var _ io.WriteCloser = (*dailyLogWriter)(nil)
