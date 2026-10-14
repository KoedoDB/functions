// Command supervisor is the KoedoDB function supervisor (internal/supervisor).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/koedodb/functions/internal/supervisor"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := supervisor.ConfigFromEnv(os.LookupEnv, supervisor.CgroupMemoryMiB)
	if err != nil {
		log.Error("the supervisor does not start", "error", err)
		os.Exit(2)
	}
	s, err := supervisor.New(cfg, log)
	if err != nil {
		log.Error("the supervisor does not start", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log.Info("the supervisor listens", "address", cfg.Listen, "functions", cfg.Functions, "budget_mib", cfg.BudgetMiB, "heap_mb", cfg.HeapMB, "max_concurrent", cfg.MaxConcurrent)
	if err := s.Run(ctx); err != nil {
		log.Error("the supervisor stopped", "error", err)
		os.Exit(1)
	}
}
