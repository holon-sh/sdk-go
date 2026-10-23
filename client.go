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
)

func Connect(ctx context.Context, name string) (*mcp.ClientSession, error) {
	scope, ok := ctx.Value(scopeKey{}).(*requestScope)
	if !ok || scope.ctx.Err() != nil {
		return nil, errors.New("dependency calls require an active incoming Holon request")
	}
	endpoint := os.Getenv(strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_MCP_URL")
	if name == "" || endpoint != scope.host+"/peer/"+url.PathEscape(name) {
		return nil, fmt.Errorf("dependency %q is not bound to the Holon host", name)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "holon-go", Version: "0.1.0"}, &mcp.ClientOptions{MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}})
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: peerTransport{scope: scope, endpoint: endpoint}, CheckRedirect: noRedirect},
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
