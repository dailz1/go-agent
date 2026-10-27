package mcpbridge

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrUnknownServer reports a serverID the Manager does not manage.
var ErrUnknownServer = fmt.Errorf("mcp: unknown server")

// ServerView is an inventory snapshot of one managed server: plain copied
// values only — no SDK sessions, transports, or tool objects. ToolNames is
// sorted and reflects current registry ownership.
type ServerView struct {
	ServerID  string
	ToolNames []string
}

// Manager orchestrates multiple Bridge instances sharing one registry: it
// records which server owns which tools, so callers get failure attribution
// (OwnerOf), inventory views (Snapshot), and per-server lifecycle without
// holding Bridge handles themselves. It adds bookkeeping only — the Agent
// runtime sees the same registry with no notion of tool origin.
//
// All Manager methods are safe for concurrent use; lifecycle operations
// (Connect, Disconnect, Refresh) are serialized against each other. Calls
// and Close remain subject to the Bridge contracts: Disconnect is not
// hot-unplug — the caller must ensure no Agent run still uses the server's
// tools (Close itself drains admitted calls, see Bridge.Close).
type Manager struct {
	mu       sync.Mutex
	registry *tool.Registry
	servers  map[string]*Bridge
}

// NewManager returns a Manager publishing into registry.
func NewManager(registry *tool.Registry) *Manager {
	return &Manager{registry: registry, servers: make(map[string]*Bridge)}
}

// Connect connects one MCP server under serverID and records it. A
// duplicate serverID is rejected with an error; a failed Connect leaves no
// entry (Bridge.Connect already unregisters its partial registrations), so
// a failed attempt reserves no ID and a valid retry on the same ID
// succeeds.
func (m *Manager) Connect(ctx context.Context, serverID string, transport sdk.Transport, cfg Config, opts ...Option) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.servers[serverID]; exists {
		return fmt.Errorf("mcp: server %q already managed", serverID)
	}
	bridge, err := Connect(ctx, m.registry, transport, cfg, opts...)
	if err != nil {
		return fmt.Errorf("mcp: connect server %q: %w", serverID, err)
	}
	m.servers[serverID] = bridge
	return nil
}

// Disconnect tears down and forgets the named server: Bridge.Close drains
// in-flight calls, unregisters still-owned names, then closes the session;
// only the session error is reported. The entry is removed even when the
// session error is non-nil — a failed close does not leave a manageable
// server behind. Unknown serverIDs report ErrUnknownServer.
func (m *Manager) Disconnect(serverID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bridge, ok := m.servers[serverID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownServer, serverID)
	}
	delete(m.servers, serverID)
	return bridge.Close()
}

// Refresh re-lists the named server's tools and reconciles its registry
// entries, returning the Bridge's summary. OwnerOf and Snapshot reflect the
// refreshed ownership on their next call — ownership is resolved live, not
// cached. Unknown serverIDs report ErrUnknownServer.
func (m *Manager) Refresh(ctx context.Context, serverID string) (*RefreshSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bridge, ok := m.servers[serverID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownServer, serverID)
	}
	return bridge.Refresh(ctx)
}

// Snapshot returns one view per managed server, sorted by serverID.
func (m *Manager) Snapshot() []ServerView {
	m.mu.Lock()
	defer m.mu.Unlock()
	views := make([]ServerView, 0, len(m.servers))
	for id := range m.servers {
		views = append(views, ServerView{ServerID: id, ToolNames: m.servers[id].Names()})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ServerID < views[j].ServerID })
	return views
}

// OwnerOf reports which managed server currently owns the registry name —
// ownership is resolved live against each bridge's identity-checked owned
// set, so names taken over by a user Replace are not attributed to the
// bridge that registered them first. It is an inventory query: an Execute
// already holding a handle may complete after its owner was disconnected,
// and historical attribution is not provided.
func (m *Manager) OwnerOf(toolName string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.servers))
	for id := range m.servers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, name := range m.servers[id].Names() {
			if name == toolName {
				return id, true
			}
		}
	}
	return "", false
}
