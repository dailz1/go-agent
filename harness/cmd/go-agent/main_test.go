package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRunHelp(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(
				[]string{arg},
				streams{in: os.Stdin, out: &out, errOut: &errOut},
				func(string) string { return "" },
			)
			if code != 0 || errOut.Len() != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
			}
			for _, flag := range []string{"-provider", "-model", "-base-url", "-api-key-env", "-no-approval"} {
				if !strings.Contains(out.String(), flag) {
					t.Errorf("help omits flag %s", flag)
				}
			}
		})
	}
}

func TestRunNonTerminal(t *testing.T) {
	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var out, errOut bytes.Buffer
	code := run(
		[]string{},
		streams{in: in, out: &out, errOut: &errOut},
		func(string) string { return "" },
	)
	if code != 0 || out.Len() == 0 || errOut.Len() != 0 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("non-terminal output contains terminal control sequences")
	}
}

func TestRunUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(
		[]string{"--unknown"},
		streams{in: os.Stdin, out: &out, errOut: &errOut},
		func(string) string { return "" },
	)
	if code != 2 || out.Len() != 0 || errOut.Len() == 0 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out.String(), errOut.String())
	}
}
