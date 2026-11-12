package holon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Environment proxy settings would send a peer's cluster-local address elsewhere.
var direct = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

func Connect(ctx context.Context, name string) (*mcp.ClientSession, error) {
	prefix := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	endpoint := os.Getenv(prefix + "_MCP_URL")
	if endpoint == "" {
		return nil, fmt.Errorf("dependency %q is not bound: set %s_MCP_URL", name, prefix)
	}
	scope, _ := ctx.Value(scopeKey{}).(*requestScope)
	httpClient := &http.Client{Transport: peerTransport{scope: scope, token: os.Getenv(prefix + "_MCP_TOKEN")}, CheckRedirect: noRedirect}
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
		r.Header.Set("X-Holon-Context", t.scope.token)
	}
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return direct.RoundTrip(r)
}
