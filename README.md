# Holon Go SDK

[![CI](https://github.com/holon-sh/sdk-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/holon-sh/sdk-go/actions/workflows/ci.yml)
[![Docs](https://img.shields.io/badge/docs-holonlabs.ai-blue)](https://holonlabs.ai/docs/)
[![Go Reference](https://pkg.go.dev/badge/github.com/holon-sh/sdk-go.svg)](https://pkg.go.dev/github.com/holon-sh/sdk-go)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go)](https://go.dev/doc/install)
[![Apache 2.0 License](https://img.shields.io/badge/license-Apache--2.0-green)](LICENSE)
[![Discord](https://img.shields.io/badge/Discord-join-5865F2?logo=discord&logoColor=white)](https://discord.gg/jXExyBdy)

This repository contains the Go runtime for [`holon`](https://github.com/holon-sh/cli).

## Install

```sh
go get github.com/holon-sh/sdk-go@latest
```

## Serving an app

```go
server := mcp.NewServer(&mcp.Implementation{Name: "foo", Version: "0.1.0"}, nil)
// Add the app's tools to server.
if err := holon.Serve(ctx, server, holon.WithFrontend("web/dist")); err != nil {
	log.Fatal(err)
}
```

`Serve` listens on `HOST` and `PORT`, 127.0.0.1:3000 unless they are set. The MCP endpoint is at `/mcp` and `/mcp/<app>`, and the files of the frontend directory are at every other path. Nothing hidden is served, nor anything a symlink leads to outside the directory. On SIGTERM, `Serve` stops accepting connections, lets the requests in flight finish, and returns.

## The caller

`holon.Context` reads who called a tool:

```go
who := holon.Context(ctx)
// who.Principal is the user Holon admitted, or nil when the request is anonymous.
// who.Caller is the app of the same session that called, or nil when a person called directly.
```

Inside Holon, where `HOLON_PLATFORM=1`, both come from the headers Holon's mesh sets once it has verified the caller. Anywhere else every request is anonymous. Either way, a tool never sees `X-Holon-*` or `Authorization` headers. A dependency called while the request lasts acts for the same user. One called after the request ends, such as from a goroutine the tool started, acts as the app.

## Invoking MCP Servers

Declare dependencies under `sh.holon.requires` in a [`server.json`](https://modelcontextprotocol.io/registry/quickstart#step-4-create-server-json) beside the server, here the public [Hugging Face](https://huggingface.co/docs/hub/en/hf-mcp-server) MCP server:

```json
{
  "name": "com.example/foo",
  "version": "0.1.0",
  "description": "Example Foo",
  "_meta": {
    "io.modelcontextprotocol.registry/publisher-provided": {
      "sh.holon": {
        "requires": {
          "huggingface": { "url": "https://huggingface.co/mcp" }
        }
      }
    }
  }
}
```

### Type-checking MCP invocations

```sh
holon types generate
go test ./...
```

A wrong tool name or argument is a type error:

```go
import apps "example.com/foo/holon"

query, limit := "llama", "3"

apps.Huggingface.HubRepoSearch(ctx, apps.HuggingfaceHubRepoSearchInput{Query: &query})

apps.Huggingface.HubRepoSearch(ctx, apps.HuggingfaceHubRepoSearchInput{Limit: &limit})
// cannot use &limit (value of type *string) as *"encoding/json".Number value in struct literal

apps.Huggingface.HubSearch(ctx, apps.HuggingfaceHubRepoSearchInput{Query: &query})
// apps.Huggingface.HubSearch undefined (type "example.com/foo/holon"._Huggingface has no field or method HubSearch)
```

## Community

Ask questions and share what you're building on the [Holon Discord](https://discord.gg/jXExyBdy).
