# Holon Go SDK

[![CI](https://github.com/holon-sh/sdk-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/holon-sh/sdk-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/holon-sh/sdk-go.svg)](https://pkg.go.dev/github.com/holon-sh/sdk-go)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go)](https://go.dev/doc/install)
[![Apache 2.0 License](https://img.shields.io/badge/license-Apache--2.0-green)](LICENSE)
[![Discord](https://img.shields.io/badge/Discord-join-5865F2?logo=discord&logoColor=white)](https://discord.gg/jXExyBdy)

This repository contains the Go runtime for [`holon`](https://github.com/holon-sh/cli).

## Install

```sh
go get github.com/holon-sh/sdk-go@latest
```

## Invoking MCP Servers

The Holon Go SDK allows generation of type-safe MCP clients. See [Example](https://github.com/holon-sh/cli#example) for an explanation, or [Dependencies](https://github.com/holon-sh/cli#dependencies) to understand dependency resolution.

### Type-checking MCP invocations

```sh
holon types generate
go test ./...
```

Clients are generated into the project's `holon/` package.

<details open>
<summary><h3>Call a tool</h3></summary>

```go
import apps "example.com/foo/holon"

func greet(ctx context.Context, name string) (string, error) {
	greeting, err := apps.Bar.Greet(ctx, apps.BarGreetInput{Name: name})
	if err != nil {
		return "", err
	}
	return greeting.Result, nil
}
```

Arguments are validated before sending; the result is validated against the output schema and returned.

</details>

<details open>
<summary><h3>Handle errors</h3></summary>

```go
var failure *holon.ToolCallError
if errors.As(err, &failure) {
	failure.Code   // "tool_error" or "input_required"
	failure.Result // the original MCP result
}
```

</details>

<details open>
<summary><h3>Read the whole result</h3></summary>

`CallTool` returns the full MCP result instead of an error:

```go
result, err := apps.Bar.CallTool(ctx, &mcp.CallToolParams{
	Name:      "greet",
	Arguments: apps.BarGreetInput{Name: "world"},
})
```

</details>

<details open>
<summary><h3>Answer a server's question</h3></summary>

A tool can stop and ask for input through an [MCP elicitation](https://modelcontextprotocol.io/specification/2026-07-28/client/elicitation). Pass a handler to answer in place:

```go
result, err := apps.Bar.CallTool(ctx, params, func(ctx context.Context, question *mcp.ElicitParams) (*mcp.ElicitResult, error) {
	answer, err := ask(ctx, question.Message)
	return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"name": answer}}, err
})
```

Without a handler, the question arrives on `ToolCallError.Result`, or as an `input_required` result from `CallTool`. To answer later, for example in another request, call again with `InputResponses` and `RequestState`.

</details>

## Serve under Holon

Register typed tools with the [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) and pass the server to `holon.Serve`. Run the executable under Holon's host, which supplies `HOLON_HOST_URL`.

```go
server := mcp.NewServer(&mcp.Implementation{Name: "foo", Version: "1.0.0"}, nil)
err := holon.Serve(ctx, server)
```

To serve a built view as the app's page, name its directory, relative to the application:

```go
err := holon.Serve(ctx, server, holon.WithFrontend("dist/web"))
```

Inside a tool handler, `holon.Context(ctx).Principal` is the admitted caller, or nil for anonymous requests. `holon.Connect(ctx, "bar")` opens an MCP client session to a declared dependency for the current request.

## Community

Ask questions and share what you're building on the [Holon Discord](https://discord.gg/jXExyBdy).
