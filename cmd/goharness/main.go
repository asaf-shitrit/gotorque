package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"example.com/go-agent-optimizer/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := cli.New(cli.Dependencies{Stdout: os.Stdout, Stderr: os.Stderr}).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
