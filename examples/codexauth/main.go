// Command codexauth logs in to an independent Codex subscription credential store.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"time"

	"github.com/dailz1/go-agent/codexauth"
	"github.com/dailz1/go-agent/llm/codex/auth"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, "Usage: codexauth login [--headless] [--store PATH]\n       codexauth status [--store PATH]")
		return err
	}
	command := args[0]
	if command != "login" && command != "status" {
		return fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("store", "", "independent credential file (default ~/.go-agent/codex/auth.json)")
	headless := flags.Bool("headless", false, "print SSH port-forward instructions without opening a browser")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *path == "" {
		resolved, err := codexauth.DefaultPath()
		if err != nil {
			return err
		}
		*path = resolved
	}
	if command == "status" {
		state, err := codexauth.Load(*path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return printStatus(out, state)
	}
	cfg := codexauth.LoginConfig{Path: *path, Output: stderr, Headless: *headless}
	if !*headless {
		cfg.OnAuthorize = openBrowser
	}
	state, err := codexauth.Login(ctx, cfg)
	if err != nil {
		return err
	}
	return printStatus(out, state)
}

func printStatus(out io.Writer, state auth.State) error {
	var account string
	if state.Token.AccountID != "" {
		sum := sha256.Sum256([]byte(state.Token.AccountID))
		account = hex.EncodeToString(sum[:4])
	}
	return json.NewEncoder(out).Encode(struct {
		Account    string    `json:"account"`
		ExpiresAt  time.Time `json:"expires_at"`
		NeedsLogin bool      `json:"needs_login"`
	}{
		Account: account, ExpiresAt: state.Token.ExpiresAt,
		NeedsLogin: state.Token.AccessToken == "" || !state.Token.ExpiresAt.After(time.Now().Add(time.Minute)),
	})
}

func openBrowser(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", address)
	case "windows":
		command = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", address)
	default:
		command = exec.CommandContext(ctx, "xdg-open", address)
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}
