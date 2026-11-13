package holon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Environment proxy settings would send a peer's cluster-local address elsewhere.
var direct = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

func Connect(ctx context.Context, name string) (*mcp.ClientSession, error) {
	variable := protocol.DependencyEnvName(name)
	endpoint := os.Getenv(variable)
	if endpoint == "" {
		return nil, fmt.Errorf("dependency %q is not bound: set %s", name, variable)
	}
	scope, _ := ctx.Value(scopeKey{}).(*requestScope)
	token := os.Getenv(strings.TrimSuffix(variable, "URL") + "TOKEN")
	httpClient := &http.Client{Transport: peerTransport{scope: scope, token: token}, CheckRedirect: noRedirect}
	// Every client can be asked. Servers must not assume a client answers: https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr#server-requirements-basic-workflow
	client := mcp.NewClient(&mcp.Implementation{Name: "holon-go", Version: "0.1.0"}, &mcp.ClientOptions{
		Capabilities:   &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{}}},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
}

func noRedirect(*http.Request, []*http.Request) error {
	return errors.New("MCP endpoint redirects are not allowed")
}

// peerTransport sends the context of the request a call is made in while that request lasts. Without one the call is
// the app's own, which the mesh admits as the app. Holon writes every <NAME>_MCP_URL inside it, so each is a peer.
type peerTransport struct {
	scope *requestScope
	token string
}

func (t peerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.scope != nil && t.scope.token != "" && t.scope.ctx.Err() == nil {
		r.Header.Set(runtimecontract.ContextHeader, t.scope.token)
	}
	if t.token != "" {
		r.Header.Set(protocol.HeaderAuthorization, protocol.BearerPrefix+t.token)
	}
	response, err := direct.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode < 400 || contentType != "application/json" {
		return response, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, mcp.DefaultMaxLineLength+1))
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *jsonrpc.Error  `json:"error"`
	}
	if err == nil && len(body) <= mcp.DefaultMaxLineLength && json.Unmarshal(body, &message) == nil && message.JSONRPC == "2.0" && message.Error != nil &&
		(len(message.ID) == 0 || string(message.ID) == "null") && r.GetBody != nil {
		original, openErr := r.GetBody()
		if openErr == nil {
			var request struct {
				ID json.RawMessage `json:"id"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(original, mcp.DefaultMaxLineLength)).Decode(&request)
			_ = original.Close()
			if decodeErr == nil && len(request.ID) != 0 && string(request.ID) != "null" {
				message.ID = request.ID
				body, _ = json.Marshal(message)
				_ = response.Body.Close()
				response.Body = io.NopCloser(bytes.NewReader(body))
				response.ContentLength = int64(len(body))
				response.Header.Del("Content-Length")
				return response, nil
			}
		}
	}
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), response.Body), response.Body}
	return response, nil
}
