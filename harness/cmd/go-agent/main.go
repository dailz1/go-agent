// Command go-agent starts the harness terminal application.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/dailz1/go-agent/harness/internal/app"
	"github.com/dailz1/go-agent/harness/internal/config"
	"github.com/dailz1/go-agent/harness/internal/tui"
)

type streams struct {
	in     *os.File
	out    io.Writer
	errOut io.Writer
}

func main() {
	os.Exit(run(
		os.Args[1:],
		streams{in: os.Stdin, out: os.Stdout, errOut: os.Stderr},
		os.Getenv,
	))
}

func run(args []string, stdio streams, getenv func(string) string) int {
	cfg, err := config.Parse(args, getenv)
	if errors.Is(err, flag.ErrHelp) {
		return printHelp(stdio.out)
	}
	if err != nil {
		fmt.Fprintln(stdio.errOut, err)
		return 2
	}
	if !tui.IsTerminal(stdio.in, stdio.out) {
		return printHelp(stdio.out)
	}
	if err := cfg.ValidateInteractive(); err != nil {
		fmt.Fprintln(stdio.errOut, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	var ui app.UI = tui.Terminal{Input: stdio.in, Output: stdio.out}
	// TODO(B-D): assemble provider, gated tools and durable controller here.
	// Stage A deliberately starts no worker and accepts no task input.
	if err := ui.Run(ctx, app.State{ApprovalRequired: !cfg.NoApproval}); err != nil {
		fmt.Fprintln(stdio.errOut, err)
		return 1
	}
	return 0
}

func printHelp(out io.Writer) int {
	if _, err := fmt.Fprint(out, config.Help()); err != nil {
		return 1
	}
	return 0
}
