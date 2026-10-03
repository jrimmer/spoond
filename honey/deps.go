// Package honey holds the flightplan engine (docs/plans/2026-10-02-honey-workflows.md).
//
// deps.go pins the modules the engine's packages will import, so they are
// in go.mod and in the worker image's module cache before the packages
// that use them exist. Delete this file once every module below has a real
// importer.
package honey

import (
	_ "cel.dev/cel-go/cel"
	_ "cel.dev/cel-go/ext"
	_ "github.com/bmatcuk/doublestar/v4"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/robfig/cron/v3"
	_ "github.com/santhosh-tekuri/jsonschema/v6"
)
