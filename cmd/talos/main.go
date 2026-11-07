// Command talos is a stdio MCP server that runs OpenCode sessions as sub-agents
// of an orchestrator such as Claude Code.
package main

import (
	"fmt"
	"os"

	"go.xchunk.org/argon"
)

func main() {
	if err := argon.New("talos").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "talos:", err)
		os.Exit(1)
	}
}
