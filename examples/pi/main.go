package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"

	acpagent "github.com/normahq/go-adk-acpagent"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	agentRuntime, err := acpagent.New(acpagent.Config{
		Context:    context.Background(),
		Command:    []string{"npx", "-y", "pi-acp"},
		WorkingDir: "/workspace",
		SessionConfig: []acpagent.SessionConfigValue{
			{ID: "thought_level", Value: "medium"},
		},
		Logger: logger,
		Stderr: io.Discard,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := agentRuntime.Close(); err != nil {
			log.Printf("close ACP agent: %v", err)
		}
	}()

	// Pass agentRuntime to an ADK runner.
}
