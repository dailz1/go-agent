// Command agentcli is a minimal interactive coding assistant built on the
// go-agent library. It demonstrates the full consumer surface: tools, the
// approval gate, streaming events, retry, compaction, and durable threads
// that survive a restart or a crash.
//
// Configuration (environment):
//
//	OPENAI_API_KEY   required
//	OPENAI_BASE_URL  optional, any OpenAI-compatible endpoint
//	OPENAI_MODEL     optional, default "gpt-4o-mini"
//	AGENTCLI_DATA    optional, thread storage directory (default ".agentcli")
//
// Usage:
//
//	go run ./examples/agentcli                  # start a new session
//	go run ./examples/agentcli --resume <id>    # continue a saved thread
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/dailz1/go-agent/pkg/agent"
	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/llm/openai"
	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

const systemPrompt = "You are a helpful coding assistant working inside the user's current directory. " +
	"Use the provided tools to read, list, and modify files when asked. " +
	"Before overwriting or deleting anything the user did not explicitly name, ask for confirmation. " +
	"Be concise."

func main() {
	resume := flag.String("resume", "", "thread ID to continue")
	flag.Parse()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "agentcli: set OPENAI_API_KEY (and optionally OPENAI_BASE_URL, OPENAI_MODEL)")
		os.Exit(1)
	}
	model := cmpOr(os.Getenv("OPENAI_MODEL"), "gpt-4o-mini")
	dataDir := cmpOr(os.Getenv("AGENTCLI_DATA"), ".agentcli")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fatal(err)
	}

	st, err := store.NewJSONL(dataDir)
	if err != nil {
		fatal(err)
	}

	registry := tool.NewRegistry()
	registerTools(registry)

	provider := openai.NewProvider(apiKey, model, providerOptions()...)
	agentInstance := agent.New(provider, registry,
		agent.WithStore(st),
		agent.WithSystemPrompt(systemPrompt),
		agent.WithApprovalFn(approve),
	)

	// Ctrl+C cancels the in-flight run: the round aborts, but the durable
	// thread stays resumable with --resume.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	threadID := *resume
	if threadID == "" {
		hintLastThread(dataDir)
	}
	fmt.Fprintf(os.Stderr, "agentcli ready (model %s, threads in %s) — type exit to quit\n", model, dataDir)

	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := in.ReadString('\n')
		if err != nil {
			break // EOF
		}
		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}

		var aborted bool
		if threadID == "" {
			threadID, aborted = runNew(ctx, agentInstance, input)
		} else {
			aborted = runOnThread(ctx, agentInstance, threadID, input)
		}
		if aborted {
			break
		}
		if threadID != "" {
			_ = os.WriteFile(filepath.Join(dataDir, "last-thread"), []byte(threadID), 0o644)
		}
	}
}

// runNew starts a fresh session on a kernel-generated thread. The real
// thread ID arrives on the terminal DoneEvent.
func runNew(ctx context.Context, a *agent.Agent, input string) (string, bool) {
	threadID := ""
	seq, err := a.RunStream(ctx, input)
	if err != nil {
		fatal(err)
	}
	for ev, err := range seq {
		if err != nil {
			return threadID, roundFailed(err)
		}
		switch e := ev.(type) {
		case agent.DoneEvent:
			threadID = e.ThreadID
			finishRound(e)
		default:
			printEvent(ev)
		}
	}
	return threadID, false
}

// runOnThread continues a known thread. If its last run never reached a
// terminal state (crash, Ctrl+C, dropped connection), ResumeThread finishes
// it instead — the model is told what stayed unresolved.
func runOnThread(ctx context.Context, a *agent.Agent, threadID, input string) bool {
	seq, err := a.RunThreadStream(ctx, threadID, input)
	if err != nil {
		if errors.Is(err, agent.ErrRunIncomplete) {
			fmt.Fprintf(os.Stderr, "[resume] thread %s has an interrupted run; finishing it first\n", threadID)
			res, rerr := a.ResumeThread(ctx, threadID)
			if rerr != nil {
				return roundFailed(rerr)
			}
			fmt.Println(messageText(res.Message))
			return false
		}
		return roundFailed(err)
	}
	for ev, err := range seq {
		if err != nil {
			return roundFailed(err)
		}
		switch e := ev.(type) {
		case agent.DoneEvent:
			if e.ThreadID != threadID {
				fatal(fmt.Errorf("thread id changed mid-run: %s -> %s", threadID, e.ThreadID))
			}
			finishRound(e)
		default:
			printEvent(ev)
		}
	}
	return false
}

// printEvent renders one streaming event. Answer text goes to stdout;
// everything observability-shaped goes to stderr so answers stay clean.
func printEvent(ev agent.AgentEvent) {
	switch e := ev.(type) {
	case agent.TextDeltaEvent:
		fmt.Print(e.Text)
	case agent.ThinkingDeltaEvent:
		// Extended-thinking fragments are shown but kept out of the answer.
		fmt.Fprintf(os.Stderr, "…%s", e.Text)
	case agent.ToolCallEvent:
		fmt.Fprintf(os.Stderr, "\n[tool] %s(%s)\n", e.Name, compactJSON(e.Args))
	case agent.ToolResultEvent:
		status := "ok"
		if e.Result != nil && e.Result.IsError() {
			status = "error"
		}
		fmt.Fprintf(os.Stderr, "[tool] %s -> %s\n", e.Name, status)
	case agent.RetryEvent:
		fmt.Fprintf(os.Stderr, "[retry %d/%d] %s\n", e.Attempt, e.MaxAttempts, e.Reason)
	case agent.CompactionEvent:
		fmt.Fprintf(os.Stderr, "[compaction] %d -> %d runes\n", e.BeforeRunes, e.AfterRunes)
	}
}

func finishRound(e agent.DoneEvent) {
	fmt.Println()
	fmt.Fprintf(os.Stderr, "[done] tools=%d truncated=%v\n", e.ToolCalls, e.Truncated)
	if e.ThreadID != "" {
		fmt.Fprintf(os.Stderr, "[thread] %s\n", e.ThreadID)
	}
}

func roundFailed(err error) bool {
	fmt.Fprintf(os.Stderr, "[error] %v\n", err)
	fmt.Fprintf(os.Stderr, "(the durable thread is unchanged; retry, or resume with the same thread id)\n")
	return false
}

var stdinReader = bufio.NewReader(os.Stdin)

// approve is the human gate for tools marked RequiresApproval.
func approve(info tool.ToolInfo, args json.RawMessage) bool {
	fmt.Fprintf(os.Stderr, "\n[approval] %s %s\nallow? (y/N) ", info.Name, compactJSON(args))
	line, err := stdinReader.ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes"
}

func providerOptions() []openai.ProviderOption {
	var opts []openai.ProviderOption
	if base := os.Getenv("OPENAI_BASE_URL"); base != "" {
		opts = append(opts, openai.WithBaseURL(base))
	}
	return opts
}

func hintLastThread(dataDir string) {
	id, err := os.ReadFile(filepath.Join(dataDir, "last-thread"))
	if err == nil && len(id) > 0 {
		fmt.Fprintf(os.Stderr, "last session: %s (continue with --resume %s)\n", string(id), string(id))
	}
}

func compactJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func messageText(m llm.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func fatal(err error) {
	slog.Error("agentcli", "error", err)
	os.Exit(1)
}
