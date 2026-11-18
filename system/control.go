// Package system holds the clients of what Holon gives every app, which an app calls without declaring it.
package system

import (
	"context"
	_ "embed"
	"encoding/json"

	holon "github.com/holon-sh/sdk-go"
)

// contract is a verbatim copy of schema/control.json in github.com/holon-sh/holon, the contract control lists. A tool's
// result never changes, and new behavior is a new tool, so the copy stays true.
//
//go:embed control.json
var contract []byte

type _dependency = holon.Dependency

// Control reads the app's own MCP calls from Holon, at CONTROL_MCP_URL.
var Control = _Control{&holon.Dependency{Name: "control", Tools: tools()}}

type _Control struct{ *_dependency }

func (client _Control) GetTraces(ctx context.Context, input ControlGetTracesInput) (ControlGetTracesOutput, error) {
	return holon.Call[ControlGetTracesOutput](ctx, client._dependency, "get_traces", input)
}

type ControlGetTracesInput = struct {
	Before *string         `json:"before,omitempty"`
	Limit  *json.Number    `json:"limit,omitempty"`
	Since  *string         `json:"since,omitempty"`
	Status json.RawMessage `json:"status,omitempty"`
	Tool   *string         `json:"tool,omitempty"`
}

type ControlGetTracesOutput = struct {
	App     *string `json:"app,omitempty"`
	Session string  `json:"session"`
	Traces  []struct {
		App        *string         `json:"app,omitempty"`
		DurationMs json.Number     `json:"durationMs"`
		ErrorCode  *json.Number    `json:"errorCode,omitempty"`
		Method     string          `json:"method"`
		Start      string          `json:"start"`
		Status     json.RawMessage `json:"status"`
		Tool       *string         `json:"tool,omitempty"`
		TraceId    string          `json:"traceId"`
	} `json:"traces"`
}

func tools() map[string]holon.ToolSchema {
	var document struct {
		Tools []struct {
			Name         string          `json:"name"`
			InputSchema  json.RawMessage `json:"inputSchema"`
			OutputSchema json.RawMessage `json:"outputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(contract, &document); err != nil {
		panic(err)
	}
	schemas := map[string]holon.ToolSchema{}
	for _, tool := range document.Tools {
		schemas[tool.Name] = holon.ToolSchema{Input: string(tool.InputSchema), Output: string(tool.OutputSchema)}
	}
	return schemas
}
