package tools

import (
	"context"

	"go.xchunk.org/argon"
	"go.xchunk.org/talos/internal/opencode"
)

// ServerControl is the part of the OpenCode connection these tools need.
// *opencode.Connection implements it.
type ServerControl interface {
	Status(ctx context.Context) opencode.Status
	Start(ctx context.Context) (opencode.Info, error)
}

type serverOutput struct {
	Reachable bool   `json:"reachable"`
	URL       string `json:"url,omitempty"`
	Version   string `json:"version,omitempty"`
	Source    string `json:"source"`
	Error     string `json:"error,omitempty"`
}

func serverStatusOf(status opencode.Status) serverOutput {
	out := serverOutput{Reachable: status.Reachable, URL: status.URL, Version: status.Version, Source: status.Source}
	if status.Err != nil {
		out.Error = status.Err.Error()
	}
	return out
}

// RegisterServer adds the tools that check and start the OpenCode server.
func RegisterServer(s *argon.Server, control ServerControl) {
	argon.Tool(s, "server_status",
		"Check whether the OpenCode server is reachable, without starting it.",
		func(ctx context.Context, _ struct{}) (serverOutput, error) {
			return serverStatusOf(control.Status(ctx)), nil
		}, argon.ReadOnly())

	argon.Tool(s, "start_server",
		"Start the managed OpenCode service if it is not running, and report its status. Harmless if it already runs. "+
			"start_task normally does this on its own, unless TALOS_AUTOSTART is off.",
		func(ctx context.Context, _ struct{}) (serverOutput, error) {
			if _, err := control.Start(ctx); err != nil {
				return serverOutput{}, err
			}
			return serverStatusOf(control.Status(ctx)), nil
		}, argon.Idempotent())
}
