package holon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"

	holon "github.com/holon-sh/sdk-go"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func addDependencyChecks(peer, app *mcp.Server) {
	var calls atomic.Int32
	replies := map[string]string{
		"incompatible": `{"structuredContent":{"id":1}}`,
		"error":        `{"resultType":"complete","isError":true,"content":[{"type":"text","text":"specific failure"}],"_meta":{"trace":"kept"}}`,
		"input":        `{"resultType":"input_required","requestState":"resume-me","inputRequests":{},"_meta":{"trace":"kept"}}`,
	}
	peer.AddTool(&mcp.Tool{Name: "contract", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		var input struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(request.Params.Arguments, &input); err != nil {
			return nil, err
		}
		if input.ID == "rpc-error" {
			return nil, &jsonrpc.Error{Code: -32001, Message: "peer rejected", Data: json.RawMessage(`{"reason":"kept"}`)}
		}
		if reply, ok := replies[input.ID]; ok {
			var result mcp.CallToolResult
			err := json.Unmarshal([]byte(reply), &result)
			return &result, err
		}
		return &mcp.CallToolResult{StructuredContent: request.Params.Arguments}, nil
	})
	dependency := &holon.Dependency{Name: "peer", Tools: map[string]holon.ToolSchema{"contract": {
		Input:  `{"type":"object","properties":{"id":{"type":"string","minLength":1},"note":{"type":["string","null"]}},"required":["id"],"additionalProperties":false}`,
		Output: `{"type":"object","properties":{"id":{"type":"string"},"note":{"type":["string","null"]}},"required":["id"],"additionalProperties":false}`,
	}}}

	type output struct {
		ID   string          `json:"id"`
		Note json.RawMessage `json:"note,omitempty"`
	}
	mcp.AddTool(app, &mcp.Tool{Name: "contracts"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		check := func() error {
			for _, note := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`"memo"`)} {
				input := output{ID: "ok", Note: note}
				got, err := holon.Call[output](ctx, dependency, "contract", input)
				if err != nil || got.ID != "ok" || string(got.Note) != string(note) {
					return fmt.Errorf("nullable result: %+v %v", got, err)
				}
			}
			before := calls.Load()
			if _, err := dependency.CallTool(ctx, &mcp.CallToolParams{Name: "contract", Arguments: map[string]any{}}); err == nil || !strings.Contains(err.Error(), "arguments") {
				return fmt.Errorf("invalid arguments accepted: %v", err)
			}
			if calls.Load() != before {
				return errors.New("invalid arguments contacted peer")
			}
			_, err := dependency.CallTool(ctx, &mcp.CallToolParams{Name: "contract", Arguments: map[string]any{"id": "rpc-error"}})
			var rpcError *jsonrpc.Error
			if !errors.As(err, &rpcError) || rpcError.Code != -32001 || string(rpcError.Data) != `{"reason":"kept"}` {
				return fmt.Errorf("lost JSON-RPC error: %v", err)
			}
			if _, err := holon.Call[output](ctx, dependency, "contract", map[string]any{"id": "incompatible"}); err == nil || !strings.Contains(err.Error(), "structured output") {
				return fmt.Errorf("incompatible result accepted: %v", err)
			}
			for mode, code := range map[string]string{"error": "tool_error", "input": "input_required"} {
				_, err := holon.Call[output](ctx, dependency, "contract", map[string]any{"id": mode})
				var failure *holon.ToolCallError
				var want mcp.CallToolResult
				if err := json.Unmarshal([]byte(replies[mode]), &want); err != nil {
					return err
				}
				want.Meta[mcp.MetaKeyServerInfo] = map[string]any{"name": "peer", "version": "1"}
				if !errors.As(err, &failure) || failure.App != "peer" || failure.Tool != "contract" || failure.Code != code || !reflect.DeepEqual(failure.Result, &want) {
					return fmt.Errorf("lost MCP result: %v", err)
				}
				raw, err := dependency.CallTool(ctx, &mcp.CallToolParams{Name: "contract", Arguments: map[string]any{"id": mode}})
				if err != nil || !reflect.DeepEqual(raw, &want) {
					return fmt.Errorf("raw response: %v", err)
				}
			}
			return nil
		}
		return nil, struct{}{}, check()
	})
}
