package holon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
		return nil, struct{}{}, errors.Join(check(), checkElicitation(ctx))
	})
	addAskingTool(peer)
}

type shipment struct {
	Action string `json:"action"`
	Size   string `json:"size,omitempty"`
}

// addAskingTool adds a tool that, like a real server, rejects a client that doesn't declare it can be asked.
func addAskingTool(peer *mcp.Server) {
	questions := map[string]mcp.InputRequestMap{
		"form":    {"size": &mcp.ElicitParams{Message: "Which size?"}},
		"url":     {"size": &mcp.ElicitParams{Message: "Pick a size.", URL: "https://sizes.example/pick", ElicitationID: "pick"}},
		"several": {"size": &mcp.ElicitParams{Message: "Second?"}, "color": &mcp.ElicitParams{Message: "First?"}},
		"endless": {"size": &mcp.ElicitParams{Message: "Again?"}},
		"busy":    {},
	}
	mcp.AddTool(peer, &mcp.Tool{Name: "ship"}, func(_ context.Context, request *mcp.CallToolRequest, input struct {
		Ask string `json:"ask"`
	}) (*mcp.CallToolResult, any, error) {
		capabilities := request.ClientCapabilities()
		if capabilities == nil || capabilities.Elicitation == nil || capabilities.Elicitation.Form == nil || capabilities.Elicitation.URL == nil {
			return nil, nil, &jsonrpc.Error{Code: mcp.CodeMissingRequiredClientCapabilities, Message: "elicitation is required"}
		}
		if request.Params.RequestState == "" || input.Ask == "endless" {
			return &mcp.CallToolResult{InputRequests: questions[input.Ask], RequestState: "asked"}, nil, nil
		}
		responses := request.Params.InputResponses
		if len(responses) != len(questions[input.Ask]) {
			return nil, nil, errors.New("a question went unanswered")
		}
		answer, ok := responses["size"].(*mcp.ElicitResult)
		if !ok {
			return nil, shipment{Action: "retried"}, nil
		}
		size, _ := answer.Content["size"].(string)
		return nil, shipment{Action: answer.Action, Size: size}, nil
	})
}

func checkElicitation(ctx context.Context) error {
	dependency := &holon.Dependency{Name: "peer", Tools: map[string]holon.ToolSchema{"ship": {Input: `{"type":"object"}`}}}
	ship := func(ask string, elicit ...holon.ElicitHandler) (shipment, error) {
		return holon.Call[shipment](ctx, dependency, "ship", map[string]any{"ask": ask}, elicit...)
	}
	accepted := &mcp.ElicitResult{Action: "accept", Content: map[string]any{"size": "M"}}
	var asked []string
	reply := func(result *mcp.ElicitResult) holon.ElicitHandler {
		return func(_ context.Context, question *mcp.ElicitParams) (*mcp.ElicitResult, error) {
			asked = append(asked, strings.TrimSpace(question.Mode+" "+question.Message+" "+question.URL))
			return result, nil
		}
	}

	for _, test := range []struct {
		ask   string
		reply *mcp.ElicitResult
		want  shipment
		asked []string
	}{
		{"form", accepted, shipment{"accept", "M"}, []string{"form Which size?"}},
		{"form", &mcp.ElicitResult{Action: "decline"}, shipment{Action: "decline"}, []string{"form Which size?"}},
		{"url", &mcp.ElicitResult{Action: "accept"}, shipment{Action: "accept"}, []string{"url Pick a size. https://sizes.example/pick"}},
		{"several", accepted, shipment{"accept", "M"}, []string{"form First?", "form Second?"}},
		{"busy", accepted, shipment{Action: "retried"}, nil},
	} {
		asked = nil
		got, err := ship(test.ask, reply(test.reply))
		if err != nil || got != test.want || !slices.Equal(asked, test.asked) {
			return fmt.Errorf("answering %s with %s: %+v, asked %q, %v", test.ask, test.reply.Action, got, asked, err)
		}
	}

	for ask, want := range map[string]string{
		"form":    "peer.ship: input_required: Which size?",
		"url":     "peer.ship: input_required: Pick a size. https://sizes.example/pick",
		"several": "peer.ship: input_required: First?: Second?",
	} {
		var failure *holon.ToolCallError
		if _, err := ship(ask); !errors.As(err, &failure) || failure.Code != "input_required" || err.Error() != want {
			return fmt.Errorf("question for %s: %v", ask, err)
		}
	}

	// A nil handler is no handler, so the question reaches the caller, who answers later.
	var failure *holon.ToolCallError
	if _, err := ship("form", nil); !errors.As(err, &failure) {
		return fmt.Errorf("deferring: %v", err)
	}
	result, err := dependency.CallTool(ctx, &mcp.CallToolParams{
		Name: "ship", Arguments: map[string]any{"ask": "form"}, InputResponses: mcp.InputResponseMap{"size": accepted}, RequestState: failure.Result.RequestState,
	})
	if err != nil || !reflect.DeepEqual(result.StructuredContent, map[string]any{"action": "accept", "size": "M"}) {
		return fmt.Errorf("answering later: %+v, %v", result, err)
	}

	asked = nil
	if _, err := ship("endless", reply(accepted)); err == nil || err.Error() != "peer.ship: input_required: Again?" || len(asked) != 10 {
		return fmt.Errorf("answer limit: %d answers, %v", len(asked), err)
	}

	unavailable := errors.New("no one to ask")
	if _, err := ship("form", func(context.Context, *mcp.ElicitParams) (*mcp.ElicitResult, error) { return nil, unavailable }); !errors.Is(err, unavailable) {
		return fmt.Errorf("handler error: %v", err)
	}

	params := &mcp.CallToolParams{Name: "ship", Arguments: map[string]any{"ask": "form"}}
	if _, err := dependency.CallTool(ctx, params, reply(accepted)); err != nil || params.RequestState != "" || params.InputResponses != nil {
		return fmt.Errorf("caller's parameters changed: %+v, %v", params, err)
	}
	return nil
}
