// Package buildinfo identifies the proxy in CLI output and both MCP sessions.
package buildinfo

// Release builds set these values with go build -ldflags=-X.
var (
	Version = "dev"
	Commit  = "unknown"
)
