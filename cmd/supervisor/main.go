// Command supervisor is the KoedoDB function supervisor for a VM with several customers. One binary, three modes:
//
//	supervisor spawner   the privileged half (root, four capabilities): the only one that holds bundles and secrets and starts workerd (internal/spawner)
//	supervisor router    the half that faces the edge, runs as an ordinary user and holds nothing (internal/router)
//	supervisor child     what a customer's process is before it becomes workerd; the spawner starts it, never run it by hand
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/koedodb/functions/internal/router"
	"github.com/koedodb/functions/internal/spawner"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: supervisor spawner|router")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	switch os.Args[1] {
	case "spawner":
		cfg, err := spawner.ConfigFromEnv(os.LookupEnv, spawner.CgroupMemoryMiB)
		if err != nil {
			log.Error("the spawner does not start", "error", err)
			os.Exit(2)
		}
		s, err := spawner.New(cfg, log)
		if err != nil {
			log.Error("the spawner does not start", "error", err)
			os.Exit(1)
		}
		log.Info("the spawner runs", "control", cfg.ControlListen, "uids", fmt.Sprintf("%d+%d", cfg.UIDBase, cfg.UIDCount), "budget_mib", cfg.BudgetMiB, "memory_mib", cfg.MemoryMiB)
		if err := s.Run(ctx); err != nil {
			log.Error("the spawner stopped", "error", err)
			os.Exit(1)
		}
	case "router":
		cfg, err := router.ConfigFromEnv(os.LookupEnv)
		if err != nil {
			log.Error("the router does not start", "error", err)
			os.Exit(2)
		}
		if os.Geteuid() == 0 {
			log.Error("the router does not start", "error", "it must not run as root: it holds nothing and needs nothing")
			os.Exit(1)
		}
		log.Info("the router listens", "address", cfg.Listen, "max_concurrent", cfg.MaxConcurrent)
		if err := router.New(cfg, log).Run(ctx); err != nil {
			log.Error("the router stopped", "error", err)
			os.Exit(1)
		}
	case "child":
		if err := spawner.ChildMain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "child:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: supervisor spawner|router")
		os.Exit(2)
	}
}
