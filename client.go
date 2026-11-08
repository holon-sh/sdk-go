package holon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// Connect opens an MCP client session to the dependency called name, at the endpoint <NAME>_MCP_URL.
// Inside a request served by [Serve], the session carries that request's authority through the Holon host.
// Outside one, <NAME>_MCP_TOKEN, if set, is sent as a bearer token.
func Connect(ctx context.Context, name string) (*mcp.ClientSession, error) {
	prefix := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	endpoint := os.Getenv(prefix + "_MCP_URL")
	httpClient := &http.Client{CheckRedirect: noRedirect}
	switch scope, ok := ctx.Value(scopeKey{}).(*requestScope); {
	case ok:
		if name == "" || endpoint != scope.host+"/peer/"+url.PathEscape(name) {
			return nil, fmt.Errorf("dependency %q is not bound to the Holon host", name)
		}
		httpClient.Transport = peerTransport{scope: scope, endpoint: endpoint}
	case endpoint == "":
		return nil, fmt.Errorf("dependency %q is not bound: set %s_MCP_URL", name, prefix)
	default:
		if token := os.Getenv(prefix + "_MCP_TOKEN"); token != "" {
			httpClient.Transport = &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}
		}
	}
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

type peerTransport struct {
	scope    *requestScope
	endpoint string
}

func (t peerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != t.endpoint || t.scope.ctx.Err() != nil {
		return nil, errors.New("dependency request is outside its active Holon scope")
	}
	for name := range r.Header {
		if credentialHeader(name) {
			return nil, errors.New("applications cannot supply Holon peer credentials")
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.scope.ctx, cancel)
	r = r.Clone(ctx)
	r.Header.Set("X-Holon-Scope", t.scope.id)
	response, err := t.scope.transport.RoundTrip(r)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	response.Body = &peerBody{ReadCloser: response.Body, close: func() { stop(); cancel() }}
	if response.Header.Get("X-Holon-Error") != "" {
		response.Body.Close()
		return nil, fmt.Errorf("holon peer rejected request: HTTP %d", response.StatusCode)
	}
	return response, nil
}

type peerBody struct {
	io.ReadCloser

	close func()
}

func (b *peerBody) Close() error {
	defer b.close()
	return b.ReadCloser.Close()
}
