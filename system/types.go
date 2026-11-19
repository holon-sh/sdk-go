// holon-generated-apps-v1 app=system
package system

import "encoding/json"

type ControlGetTracesInput = struct {
	Before *string         "json:\"before,omitempty\""
	Limit  *json.Number    "json:\"limit,omitempty\""
	Since  *string         "json:\"since,omitempty\""
	Status json.RawMessage "json:\"status,omitempty\""
	Tool   *string         "json:\"tool,omitempty\""
}

type ControlGetTracesOutput = struct {
	App     *string "json:\"app,omitempty\""
	Session string  "json:\"session\""
	Traces  []struct {
		App        *string         "json:\"app,omitempty\""
		DurationMs json.Number     "json:\"durationMs\""
		ErrorCode  *json.Number    "json:\"errorCode,omitempty\""
		Method     string          "json:\"method\""
		Start      string          "json:\"start\""
		Status     json.RawMessage "json:\"status\""
		Tool       *string         "json:\"tool,omitempty\""
		TraceId    string          "json:\"traceId\""
	} "json:\"traces\""
}
