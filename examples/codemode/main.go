// Command codemode demonstrates the codemode satellite end to end: a
// scripted provider drives one Agent whose model registry contains only the
// run_code tool; the script fans out two business tool calls, triggers the
// nested approval for one of them, and returns a tiny summary while the
// intermediate results stay inside the script.
//
// Run it with:
//
//	go run ./examples/codemode
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/codemode"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type listIssuesTool struct{}

func (t *listIssuesTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	ps.Properties["projectId"] = tool.Property{Type: "string"}
	ps.Required = []string{"projectId"}
	return tool.ToolInfo{Name: "listIssues", Description: "List issues of one project", Parameters: ps}
}

func (t *listIssuesTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var in struct {
		ProjectID string `json:"projectId"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.NewErrorResult("bad args: %v", err), nil
	}
	items := []map[string]string{{"status": "closed"}}
	if in.ProjectID == "P1" {
		items = append([]map[string]string{{"status": "open"}}, items...)
	}
	return tool.NewDataResult("found issues", map[string]any{"items": items})
}

type applyTagTool struct {
	approved map[string]bool
}

func (t *applyTagTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	ps.Properties["projectId"] = tool.Property{Type: "string"}
	ps.Properties["tag"] = tool.Property{Type: "string"}
	ps.Required = []string{"projectId", "tag"}
	return tool.ToolInfo{Name: "applyTag", Description: "Tag one project", Parameters: ps, RequiresApproval: true}
}

func (t *applyTagTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var in struct {
		ProjectID string `json:"projectId"`
		Tag       string `json:"tag"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.NewErrorResult("bad args: %v", err), nil
	}
	t.approved[in.ProjectID+"/"+in.Tag] = true
	return tool.NewTextResult("tagged " + in.ProjectID), nil
}

func main() {
	source := tool.NewRegistry()
	apply := &applyTagTool{approved: map[string]bool{}}
	if err := source.Register(&listIssuesTool{}); err != nil {
		log.Fatalf("register listIssues: %v", err)
	}
	if err := source.Register(apply); err != nil {
		log.Fatalf("register applyTag: %v", err)
	}
	model := tool.NewRegistry()

	mode, err := codemode.Connect(context.Background(), source, model, codemode.Config{},
		codemode.WithApproval(func(info tool.ToolInfo, _ json.RawMessage) bool {
			fmt.Printf("[approval] nested call approved: %s\n", info.Name)
			return true
		}),
	)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer mode.Close()

	advertised := model.List()
	fmt.Printf("[wiring] model registry advertises %d tool(s):\n", len(advertised))
	for _, info := range advertised {
		fmt.Printf("  - %s (description %d runes)\n", info.Name, len([]rune(info.Description)))
	}
	wrapperInfo := mode.ToolInfo()
	catalogVersion := mode.CatalogVersion()
	callArgs, _ := json.Marshal(map[string]string{
		"code": `const summary = [];
for (const projectId of ["P1", "P2"]) {
  const result = await tools["listIssues"]({projectId});
  const open = result.value.items.filter(issue => issue.status === "open").length;
  if (open > 0) {
    await tools["applyTag"]({projectId, tag: "needs-triage"});
  }
  summary.push({projectId, open});
}
return summary;`,
		"catalog_version": catalogVersion,
	})

	step1 := agenttest.Exchange{
		Method: agenttest.MethodChatStream,
		Request: agenttest.Request{
			Messages: []llm.Message{llm.UserMessage("check P1 and P2 issues")},
			Tools:    []tool.ToolInfo{wrapperInfo},
			Options:  llm.ApplyOptions(nil),
		},
		StreamChunks: []llm.Chunk{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-1", Name: mode.Name()},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-1", Delta: string(callArgs)},
			&llm.DoneChunk{},
		},
	}
	step2 := agenttest.Exchange{
		Method: agenttest.MethodChatStream,
		Request: agenttest.Request{
			Messages: []llm.Message{
				llm.UserMessage("check P1 and P2 issues"),
				llm.AssistantToolCallMessage(&llm.ToolUseBlock{Type: "tool_use", ID: "call-1", Name: mode.Name(), Input: callArgs}),
				llm.ToolResultMessage("call-1", &tool.ToolResult{Content: `[{"projectId":"P1","open":1},{"projectId":"P2","open":0}]`}),
			},
			Tools:   []tool.ToolInfo{wrapperInfo},
			Options: llm.ApplyOptions(nil),
		},
		StreamChunks: []llm.Chunk{
			&llm.TextDeltaChunk{Text: "P1 has 1 open issue and was tagged; P2 is clean."},
			&llm.DoneChunk{},
		},
	}
	a := agent.New(agenttest.NewScriptedProvider(step1, step2), model,
		agent.WithApprovalFn(func(info tool.ToolInfo, _ json.RawMessage) bool {
			fmt.Printf("[approval] outer code approved: %s\n", info.Name)
			return true
		}),
		agent.WithMaxIter(4),
	)
	runRes, err := a.Run(context.Background(), "check P1 and P2 issues")
	if err != nil {
		log.Fatalf("agent run: %v", err)
	}
	final := ""
	for _, block := range runRes.Message.Content {
		if text, ok := block.(*llm.TextBlock); ok {
			final += text.Text
		}
	}
	fmt.Printf("[history] final message: %s\n", final)
	fmt.Printf("[effects] applyTag side effects: %v\n", apply.approved)
	fmt.Println("[expected] P1/needs-triage present, P2/needs-triage absent (P2 has no open issues)")
}
