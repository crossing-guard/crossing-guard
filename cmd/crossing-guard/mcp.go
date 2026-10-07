package main

import (
	"context"

	"crossing-guard/internal/daemon"
	"crossing-guard/internal/memcli"
	"crossing-guard/internal/recallmcp"
)

// recallDaemon is the located daemon as the recall server reads it.
type recallDaemon struct{ location daemon.Location }

func (d recallDaemon) Addr() string { return d.location.Addr }

func (d recallDaemon) GetJSON(path string, out any) error { return d.location.GetJSON(path, out) }

func (d recallDaemon) GetJSONContext(ctx context.Context, path string, out any) error {
	return d.location.GetJSONContext(ctx, path, out)
}

// PostJSON carries the one write-shaped call (the propose door) through the
// same located, tokened transport as every read. Location's own client
// timeout bounds it.
func (d recallDaemon) PostJSON(path string, body, out any) error {
	return d.location.PostJSON(path, body, out)
}

// mcpCmd runs the recall MCP server. The daemon is located per need, never
// once at start, so a session opened before the daemon still gets answers.
func mcpCmd(args []string) int {
	return recallmcp.Main(args, func() (recallmcp.Daemon, error) {
		location, err := daemon.LocateConsole()
		if err != nil {
			return nil, err
		}
		return recallDaemon{location: location}, nil
	})
}

// locateMemoryDaemon hands the memory read verbs the same located, tokened
// client the recall server reads through (memcli asks the daemon which store
// it reads and refuses one its writes do not use).
func locateMemoryDaemon() (memcli.Daemon, error) {
	location, err := daemon.LocateConsole()
	if err != nil {
		return memcli.Daemon{}, err
	}
	return memcli.Daemon{Reader: recallDaemon{location: location}, Addr: location.Addr}, nil
}
