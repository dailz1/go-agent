package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/dailz1/go-agent/codexauth"
)

var (
	authorizationInput io.Reader = os.Stdin
	terminalInput                = func() bool { return term.IsTerminal(os.Stdin.Fd()) }
	login                        = codexauth.Login
	launchBrowser                = openBrowser
)

func chooseManualAuthorization(in io.Reader, out io.Writer, interactive bool) (bool, error) {
	if !interactive {
		_, err := fmt.Fprintln(out, "Non-TTY stdin: using manual URL authorization.")
		return true, err
	}
	if _, err := fmt.Fprintln(out, "? Choose authorization method:\n  1. Open the default browser\n  2. Copy the authorization URL (remote/SSH supported)"); err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(in)
	for {
		if _, err := fmt.Fprint(out, "Choice [1]: "); err != nil {
			return false, err
		}
		if !scanner.Scan() {
			err := scanner.Err()
			if err == nil {
				err = io.EOF
			}
			return false, fmt.Errorf("read authorization choice: %w", err)
		}
		switch strings.TrimSpace(scanner.Text()) {
		case "", "1":
			return false, nil
		case "2":
			return true, nil
		default:
			if _, err := fmt.Fprintln(out, "Enter 1 or 2."); err != nil {
				return false, err
			}
		}
	}
}
