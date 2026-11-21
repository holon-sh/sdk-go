package holon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type ToolSchema struct {
	Input, Output string
}

type Dependency struct {
	Name  string
	Tools map[string]ToolSchema
}

func validate(schema string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	document, err := jsonschema.UnmarshalJSON(strings.NewReader(schema))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	const resource = "urn:holon:contract"
	_ = compiler.AddResource(resource, document)
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return err
	}
	decoded, _ := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	return compiled.Validate(decoded)
}

// ElicitHandler answers a server's question during one tool call.
type ElicitHandler func(context.Context, *mcp.ElicitParams) (*mcp.ElicitResult, error)

func send(ctx context.Context, client *mcp.ClientSession, params mcp.CallToolParams, elicit []ElicitHandler) (*mcp.CallToolResult, error) {
	for round := 0; ; round++ {
		result, err := client.CallTool(ctx, &params)
		// A server may keep asking, so answering stops after ten rounds and the last question reaches the caller.
		if err != nil || !result.NeedsInput() || len(elicit) == 0 || elicit[0] == nil || round == 10 {
			return result, err
		}
		params.RequestState = result.RequestState
		params.InputResponses = mcp.InputResponseMap{}
		for _, key := range slices.Sorted(maps.Keys(result.InputRequests)) {
			if question, ok := result.InputRequests[key].(*mcp.ElicitParams); ok {
				if params.InputResponses[key], err = elicit[0](ctx, question); err != nil {
					return nil, err
				}
			}
		}
	}
}

func (d *Dependency) CallTool(ctx context.Context, params *mcp.CallToolParams, elicit ...ElicitHandler) (*mcp.CallToolResult, error) {
	if params == nil {
		return nil, errors.New("tool parameters are required")
	}
	tool, ok := d.Tools[params.Name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %s.%s", d.Name, params.Name)
	}
	if err := validate(tool.Input, params.Arguments); err != nil {
		return nil, fmt.Errorf("arguments for %s.%s: %w", d.Name, params.Name, err)
	}
	client, err := Connect(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	result, err := send(ctx, client, *params, elicit)
	if err != nil {
		return nil, err
	}
	if !result.IsError && !result.NeedsInput() && len(tool.Output) > 0 {
		if err := validate(tool.Output, result.StructuredContent); err != nil {
			return nil, fmt.Errorf("structured output from %s.%s: %w", d.Name, params.Name, err)
		}
	}
	return result, nil
}

type ToolCallError struct {
	App    string
	Tool   string
	Code   string
	Result *mcp.CallToolResult
}

func (e *ToolCallError) Error() string {
	message := fmt.Sprintf("%s.%s: %s", e.App, e.Tool, e.Code)
	for _, key := range slices.Sorted(maps.Keys(e.Result.InputRequests)) {
		if question, ok := e.Result.InputRequests[key].(*mcp.ElicitParams); ok {
			message = strings.TrimSpace(message + ": " + question.Message + " " + question.URL)
		}
	}
	return message
}

func Call[Output any](ctx context.Context, dependency *Dependency, name string, input any, elicit ...ElicitHandler) (Output, error) {
	var output Output
	result, err := dependency.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: input}, elicit...)
	if err != nil {
		return output, err
	}
	if result.IsError || result.NeedsInput() {
		code := "tool_error"
		if result.NeedsInput() {
			code = "input_required"
		}
		return output, &ToolCallError{App: dependency.Name, Tool: name, Code: code, Result: result}
	}
	data, _ := json.Marshal(result.StructuredContent)
	err = json.Unmarshal(data, &output)
	return output, err
}
