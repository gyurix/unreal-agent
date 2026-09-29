// Command unreal-agent-runner executes one JSON request and writes persisted session items as JSONL.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
)

func main() {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unreal-agent-runner: locate executable: %v\n", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(agentrunner.RunMain(
		ctx, os.Args[1:], os.Getenv, os.Environ,
		os.Stdin, os.Stdout, os.Stderr,
		agentrunner.Config{
			Name:           "unreal-agent-runner",
			ParseRequest:   parseRequest,
			Providers:      agentrunner.DefaultProviders(),
			TaskExecutable: executable,
		},
	))
}
