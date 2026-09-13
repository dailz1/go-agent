package mcpbridge

import (
	"os/exec"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCommandTransportCompiles(t *testing.T) {
	transport := &sdk.CommandTransport{Command: exec.Command("mcp-server")}
	if transport.Command.Path == "" {
		t.Fatal("command transport lost command")
	}
}
