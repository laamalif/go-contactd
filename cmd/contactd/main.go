package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/laamalif/go-contactd/internal/config"
	"github.com/laamalif/go-contactd/internal/db"
	"github.com/laamalif/go-contactd/internal/server"
	"golang.org/x/crypto/bcrypt"
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
	rt, err := prepareServeRuntime(context.Background(), args, currentEnvMap(), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "startup error: %v\n", err)
		return 2
	}
	defer rt.close()

	rt.logger.Info("server starting", "event", "server starting", "listen", rt.cfg.ListenAddr, "db_path", rt.cfg.DBPath)

	srv := &http.Server{
		Addr:    rt.cfg.ListenAddr,
		Handler: rt.handler,
	}

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		rt.logger.Error("listen failed", "event", "listen failed", "error", err)
		return 1
	}
	return 0
}

type serveRuntime struct {
	cfg     config.ServeConfig
	store   *db.Store
	handler http.Handler
	logger  *slog.Logger
}

func (rt *serveRuntime) close() error {
	if rt == nil || rt.store == nil {
		return nil
	}
	return rt.store.Close()
}

func prepareServeRuntime(ctx context.Context, args []string, env map[string]string, logOut io.Writer) (*serveRuntime, error) {
	cfg, err := config.LoadServeConfig(args, env)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(logOut, nil))

	store, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := startupStore(ctx, store, cfg, logger); err != nil {
		_ = store.Close()
		return nil, err
	}

	h := server.NewHandler(server.HandlerOptions{
		Logger:     logger,
		ReadyCheck: store.Ready,
		Authenticate: func(ctx context.Context, username, password string) (string, bool, error) {
			ok, _, err := store.AuthenticateUser(ctx, username, password)
			if err != nil {
				return "", false, err
			}
			if !ok {
				return "", false, nil
			}
			return username, true, nil
		},
	})

	return &serveRuntime{
		cfg:     cfg,
		store:   store,
		handler: h,
		logger:  logger,
	}, nil
}

func startupStore(ctx context.Context, store *db.Store, cfg config.ServeConfig, logger *slog.Logger) error {
	for _, seed := range cfg.Users {
		if _, err := bcrypt.Cost([]byte(seed.PasswordHash)); err != nil {
			return fmt.Errorf("invalid bcrypt hash for user %q: %w", seed.Username, err)
		}
	}

	var prunedAge int64
	if cfg.ChangeRetentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -cfg.ChangeRetentionDays)
		n, err := store.PruneCardChangesByAge(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("startup prune by age: %w", err)
		}
		prunedAge = n
	}

	var prunedMax int64
	if cfg.ChangeRetentionMaxRevisions > 0 {
		n, err := store.PruneCardChangesByMaxRevisions(ctx, cfg.ChangeRetentionMaxRevisions)
		if err != nil {
			return fmt.Errorf("startup prune by max revisions: %w", err)
		}
		prunedMax = n
	}
	logger.Info("changes pruned", "event", "changes pruned", "by_age", prunedAge, "by_max_revisions", prunedMax)

	userCount, err := store.UserCount(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if userCount > 0 && !cfg.ForceSeed {
		logger.Info("seed skipped", "event", "seed skipped", "reason", "db_non_empty")
		return nil
	}
	for _, seed := range cfg.Users {
		if err := seedUser(ctx, store, cfg, seed, cfg.ForceSeed); err != nil {
			return err
		}
		logger.Info("user seeded", "event", "user seeded", "user", seed.Username)
	}
	return nil
}

func seedUser(ctx context.Context, store *db.Store, cfg config.ServeConfig, seed config.SeedUser, force bool) error {
	userID, err := store.UserIDByUsername(ctx, seed.Username)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("lookup seed user %q: %w", seed.Username, err)
		}
		userID, err = store.CreateUser(ctx, seed.Username, seed.PasswordHash)
		if err != nil {
			return fmt.Errorf("create seed user %q: %w", seed.Username, err)
		}
	} else if force {
		if err := store.SetUserPasswordHash(ctx, userID, seed.PasswordHash); err != nil {
			return fmt.Errorf("update seed user %q hash: %w", seed.Username, err)
		}
	}

	if _, _, err := store.EnsureAddressbook(ctx, userID, cfg.DefaultBookSlug, cfg.DefaultBookName); err != nil {
		return fmt.Errorf("ensure default addressbook for %q: %w", seed.Username, err)
	}
	return nil
}

func currentEnvMap() map[string]string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				env[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return env
}
