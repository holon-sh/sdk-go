package system_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/holon-sh/sdk-go/system"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGetTraces(t *testing.T) {
	peer := mcp.NewServer(&mcp.Implementation{Name: "control", Version: "1"}, nil)
	peer.AddTool(&mcp.Tool{Name: "get_traces", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var filter struct{ Status string }
			if err := json.Unmarshal(req.Params.Arguments, &filter); err != nil {
				return nil, err
			}
			trace := map[string]any{"traceId": "a1", "start": "2026-11-18T12:00:00Z", "method": "tools/call", "status": filter.Status, "durationMs": 12}
			return &mcp.CallToolResult{StructuredContent: map[string]any{"session": "dev", "traces": []any{trace}}}, nil
		})
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer server.Close()
	t.Setenv("CONTROL_MCP_URL", server.URL)
	page, err := system.Control.GetTraces(t.Context(), system.ControlGetTracesInput{Status: json.RawMessage(`"error"`)})
	if err != nil || page.Session != "dev" || len(page.Traces) != 1 || string(page.Traces[0].Status) != `"error"` || page.Traces[0].TraceId != "a1" {
		t.Fatalf("get_traces returned %+v, %v", page, err)
	}
	if _, err := system.Control.GetTraces(t.Context(), system.ControlGetTracesInput{}); err == nil || !strings.Contains(err.Error(), "structured output from control.get_traces") {
		t.Fatalf("invalid trace status: %v", err)
	}
}
