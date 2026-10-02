//go:build !nomcp

package main

import (
	"github.com/jrimmer/spoond/v2/cmd/spoond-dev-mcp"
)

func init() {
	register(command{
		name: "mcp",
		desc: "MCP server (stdio)",
		run:  spoondmcp.Main,
	})
}
