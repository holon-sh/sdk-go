package holon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	holon "github.com/holon-sh/sdk-go"
)

func TestStandaloneCalls(t *testing.T) {
	ctx := t.Context()
	peer := mcp.NewServer(&mcp.Implementation{Name: "peer", Version: "1"}, nil)
	peer.AddTool(&mcp.Tool{Name: "whoami", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"authorization": r.Extra.Header.Get("Authorization")}}, nil
	})
	addAskingTool(peer)
	var served atomic.Int32
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer }, &mcp.StreamableHTTPOptions{Stateless: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
			return
		}
		served.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	dependency := &holon.Dependency{Name: "peer", Tools: map[string]holon.ToolSchema{"whoami": {Input: `{"type":"object"}`}}}
	whoami := func() (string, error) {
		got, err := holon.Call[map[string]string](ctx, dependency, "whoami", map[string]any{})
		return got["authorization"], err
	}

	if _, err := whoami(); err == nil || !strings.Contains(err.Error(), "PEER_MCP_URL") {
		t.Fatalf("unbound dependency: %v", err)
	}
	t.Setenv("PEER_MCP_URL", server.URL)
	if got, err := whoami(); err != nil || got != "" {
		t.Fatalf("without a token: %q, %v", got, err)
	}
	t.Setenv("PEER_MCP_TOKEN", "secret")
	if got, err := whoami(); err != nil || got != "Bearer secret" {
		t.Fatalf("with a token: %q, %v", got, err)
	}
	if err := checkElicitation(ctx); err != nil {
		t.Error(err)
	}

	before := served.Load()
	t.Setenv("PEER_MCP_URL", server.URL+"/moved")
	if _, err := whoami(); err == nil || served.Load() != before {
		t.Errorf("token followed a redirect: %v", err)
	}
	t.Setenv("HOLON_HOST_URL", "http://127.0.0.1:1/broker")
	if _, err := whoami(); err == nil || !strings.Contains(err.Error(), "require an active incoming Holon request") || served.Load() != before {
		t.Errorf("hosted call without a request: %v", err)
	}
}
