package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/laamalif/go-contactd/internal/config"
	"github.com/laamalif/go-contactd/internal/server"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: go-contactd <subcommand>")
		return 2
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:], stderr)
	case "version":
		fmt.Fprintln(stdout, "go-contactd dev")
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand: %s\n", args[0])
		fmt.Fprintln(stderr, "usage: go-contactd <subcommand>")
		return 2
	}
}

func runServe(args []string, stderr *os.File) int {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				env[kv[:i]] = kv[i+1:]
				break
			}
		}
	}

	cfg, err := config.LoadServeConfig(args, env)
	if err != nil {
		fmt.Fprintf(stderr, "config error: %v\n", err)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	logger.Info("server starting", "event", "server starting", "listen", cfg.ListenAddr, "db_path", cfg.DBPath)

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: server.NewHandler(server.HandlerOptions{Logger: logger, ReadyCheck: func(context.Context) error { return nil }}),
	}

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("listen failed", "event", "listen failed", "error", err)
		return 1
	}
	return 0
}
