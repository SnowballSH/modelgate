package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/SnowballSH/modelgate/internal/config"
	"github.com/SnowballSH/modelgate/internal/server"
	"github.com/SnowballSH/modelgate/webui"
)

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "modelgate:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := server.Run(ctx, cfg, webui.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "modelgate:", err)
		os.Exit(1)
	}
}
