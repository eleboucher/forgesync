// Command forgesync syncs issues, comments, and pull requests from mirror
// targets (GitHub, other Forgejo instances) back into your canonical Forgejo.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/config"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/health"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/syncloop"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/version"
)

func main() {
	rootCmd := &cobra.Command{
		Use:           "forgesync",
		Short:         "Sync issues, comments, and PRs from mirror targets back into Forgejo",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	defaultConfig := "configs/forgesync.yaml"
	if v := os.Getenv("FORGESYNC_CONFIG"); v != "" {
		defaultConfig = v
	}

	var (
		configPath string
		envFile    string
	)
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", defaultConfig, "config file path")
	rootCmd.PersistentFlags().StringVar(&envFile, "env-file", ".env",
		"load env vars from this file (process env wins)")

	rootCmd.AddCommand(runCmd(rootCmd, &configPath, &envFile))
	rootCmd.AddCommand(healthcheckCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runCmd(root *cobra.Command, configPath, envFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the sync engine",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := loadEnvFile(*envFile); err != nil {
				return err
			}

			// If --config wasn't explicitly set, honor FORGESYNC_CONFIG that
			// may have just been loaded from the env file.
			cfgPath := *configPath
			if !root.PersistentFlags().Changed("config") {
				if v := os.Getenv("FORGESYNC_CONFIG"); v != "" {
					cfgPath = v
				}
			}

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}

			logger := buildLogger(cfg)
			logger.Info("forgesync starting", "version", version.String(), "poll_interval", cfg.PollInterval)

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			engine, err := syncloop.New(cfg, logger)
			if err != nil {
				return err
			}
			healthSrv := health.New(cfg.HealthListen, logger)

			g, gctx := errgroup.WithContext(ctx)
			g.Go(func() error {
				if err := healthSrv.Run(gctx); err != nil {
					return fmt.Errorf("health: %w", err)
				}
				return nil
			})
			g.Go(func() error {
				if err := engine.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
					return fmt.Errorf("engine: %w", err)
				}
				return nil
			})
			return g.Wait()
		},
	}
}

func healthcheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe the local /healthz endpoint (exit 0 on healthy)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			port := healthPort(os.Getenv("FORGESYNC_HEALTH_LISTEN"))
			urlStr := "http://127.0.0.1:" + port + "/healthz"

			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()

			// URL is constructed against 127.0.0.1 with a port parsed from the
			// operator's own config; this is a self-probe by design, not SSRF.
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil) //nolint:gosec // localhost self-probe
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req) //nolint:gosec // localhost self-probe
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("status %d", resp.StatusCode)
			}
			return nil
		},
	}
}

// loadEnvFile sources an env file if it exists. Process env wins (godotenv
// does not override pre-set vars). A missing file is not an error.
func loadEnvFile(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat env file: %w", err)
	}
	if err := godotenv.Load(path); err != nil {
		return fmt.Errorf("load env file: %w", err)
	}
	return nil
}

// healthPort extracts the port from a listen address (e.g. ":8080", "0.0.0.0:8080").
// Falls back to "8080" when the input is empty or unparseable.
func healthPort(addr string) string {
	if addr == "" {
		return "8080"
	}
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "8080"
}

func buildLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.LogFormat, "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
