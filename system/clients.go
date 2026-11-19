// holon-generated-apps-v1 app=system
package system

import (
	"context"

	holon "github.com/holon-sh/sdk-go"
)

type _dependency = holon.Dependency

var Control = _Control{&holon.Dependency{Name: "control", Tools: map[string]holon.ToolSchema{
	"get_traces": {
		Input:  "{\"additionalProperties\":false,\"properties\":{\"before\":{\"description\":\"RFC 3339 time, exclusive: pass the oldest start returned to read the page before it.\",\"type\":\"string\"},\"limit\":{\"description\":\"The most calls to return. Defaults to 50, at most 200.\",\"type\":\"integer\"},\"since\":{\"description\":\"How far back to read: a duration such as 24h, or an RFC 3339 time. Defaults to 15m.\",\"type\":\"string\"},\"status\":{\"description\":\"Only the calls that ended this way.\",\"enum\":[\"ok\",\"error\"],\"type\":\"string\"},\"tool\":{\"description\":\"Only the calls to this tool.\",\"type\":\"string\"}},\"type\":\"object\"}",
		Output: "{\"additionalProperties\":false,\"properties\":{\"app\":{\"type\":\"string\"},\"session\":{\"type\":\"string\"},\"traces\":{\"items\":{\"additionalProperties\":false,\"properties\":{\"app\":{\"type\":\"string\"},\"durationMs\":{\"type\":\"integer\"},\"errorCode\":{\"type\":\"integer\"},\"method\":{\"type\":\"string\"},\"start\":{\"type\":\"string\"},\"status\":{\"enum\":[\"ok\",\"error\"],\"type\":\"string\"},\"tool\":{\"type\":\"string\"},\"traceId\":{\"type\":\"string\"}},\"required\":[\"traceId\",\"start\",\"method\",\"status\",\"durationMs\"],\"type\":\"object\"},\"type\":\"array\"}},\"required\":[\"session\",\"traces\"],\"type\":\"object\"}",
	},
}}}

type _Control struct{ *_dependency }

func (client _Control) GetTraces(ctx context.Context, input ControlGetTracesInput) (ControlGetTracesOutput, error) {
	return holon.Call[ControlGetTracesOutput](ctx, client._dependency, "get_traces", input)
}
