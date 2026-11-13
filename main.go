package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	address := flag.String("listen", ":65460", "HTTP listen address")
	e2e := flag.Bool("e2e", false, "run e2e test")
	flag.Parse()
	logs, err := newDailyLogWriter("log", 7, time.Now)
	if err != nil {
		return 1
	}
	defer logs.Close()
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	slog.SetDefault(logger)
	if *e2e {
		defaults, options, err := LoadE2EConfig(".env.e2e")
		if err != nil {
			logger.Error("failed to load e2e configuration")
			return 1
		}
		if err := RunE2E(defaults, options); err != nil {
			fmt.Fprintln(os.Stderr, err)
			logger.Error("e2e failed; see console output")
			return 1
		}
		return 0
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		logger.Error("failed to listen")
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger.Info("server started", "address", listener.Addr().String())
	if err = Serve(ctx, listener, NewServer(Options{Logger: logger})); err != nil {
		logger.Error("http server exited with error")
		return 1
	}
	return 0
}

func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	s := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if s.Shutdown(shutdown) != nil {
				s.Close()
			}
		case <-done:
		}
	}()
	err := s.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
