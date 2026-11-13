package holon_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	holon "github.com/holon-sh/sdk-go"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDependencyHTTPError(t *testing.T) {
	for _, phase := range []string{"connect", "tools/call", "correlated"} {
		t.Run(phase, func(t *testing.T) {
			peer := mcp.NewServer(&mcp.Implementation{Name: "warehouse", Version: "1"}, nil)
			var executions, rejections atomic.Int32
			peer.AddTool(&mcp.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				executions.Add(1)
				return &mcp.CallToolResult{}, nil
			})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer }, &mcp.StreamableHTTPOptions{Stateless: true})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "connect" || r.Header.Get("Mcp-Method") == "tools/call" {
					rejections.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					var request struct {
						ID json.RawMessage `json:"id"`
					}
					if phase == "correlated" {
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0", "id": request.ID,
						"error": &jsonrpc.Error{Code: -32000, Message: "Connect your warehouse account: https://holon.example/connections",
							Data: json.RawMessage(`{"connect_url":"https://holon.example/connections"}`)},
					})
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			t.Setenv("WAREHOUSE_MCP_URL", server.URL)
			dependency := &holon.Dependency{Name: "warehouse", Tools: map[string]holon.ToolSchema{"read": {Input: `{"type":"object"}`}}}
			_, err := dependency.CallTool(t.Context(), &mcp.CallToolParams{Name: "read", Arguments: map[string]any{}})
			var failure *jsonrpc.Error
			if !errors.As(err, &failure) || failure.Code != -32000 || !strings.Contains(failure.Message, "Connect your warehouse account") ||
				string(failure.Data) != `{"connect_url":"https://holon.example/connections"}` {
				t.Fatalf("connection instruction lost: %v", err)
			}
			if (phase != "connect" && rejections.Load() != 1) || executions.Load() != 0 {
				t.Fatalf("request replayed: %d rejections, %d executions", rejections.Load(), executions.Load())
			}
		})
	}
}
