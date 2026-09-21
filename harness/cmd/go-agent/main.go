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
	"github.com/dailz1/go-agent/harness/internal/session"
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
	startup, err := app.Prepare(cfg, getenv)
	if err != nil {
		fmt.Fprintln(stdio.errOut, err)
		return 2
	}
	defer startup.Close()
	manager, err := session.Open(startup.Config.DataDir)
	if err != nil {
		fmt.Fprintln(stdio.errOut, err)
		return 2
	}
	defer manager.Close()
	controller := app.NewController(startup, manager)
	defer func() { _ = controller.Close(context.Background()) }()
	sessions, err := controller.ListSessions()
	if err != nil {
		fmt.Fprintln(stdio.errOut, err)
		return 2
	}
	details := []string{
		"Provider: " + cfg.Provider + " / " + cfg.Model,
		"Workspace: " + startup.Workspace.Path(),
	}
	if cfg.NoApproval {
		details = append(details, "Approval: disabled by --no-approval")
	} else {
		details = append(details, "Approval: every side effect asks")
	}
	details = append(details, fmt.Sprintf("Sessions: %d persisted under %s", len(sessions), startup.Config.DataDir))
	details = append(details, "Interactive chat arrives in Stage E; no model is called.")
	for _, source := range startup.Rules.Snapshot().Sources {
		details = append(details, "Rules: "+source.Path+" ("+source.Hash+")")
	}
	details = append(details, startup.Notices...)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	var ui app.UI = tui.Terminal{Input: stdio.in, Output: stdio.out}
	// Stage D: the durable controller is wired and owns sessions; the
	// interactive chat surface itself is Stage E, so startup still makes no
	// model call.
	if err := ui.Run(ctx, app.State{ApprovalRequired: !cfg.NoApproval, Startup: details}); err != nil {
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
