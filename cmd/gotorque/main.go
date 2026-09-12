package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"example.com/gotorque/internal/cli"
)

func main() {
	os.Exit(run())
}

// run keeps os.Exit out of the deferred-cancel path: returning the status lets
// signal.NotifyContext's cancel run before the process exits.
func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := cli.New(cli.Dependencies{Stdout: os.Stdout, Stderr: os.Stderr}).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
