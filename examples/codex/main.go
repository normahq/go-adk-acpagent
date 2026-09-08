package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"

	acpagent "github.com/normahq/go-adk-acpagent/v2"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	agentRuntime, err := acpagent.NewWithContext(ctx, acpagent.Config{
		Command:    []string{"npx", "-y", "@normahq/codex-acp-bridge@latest"},
		WorkingDir: "/workspace",
		Logger:     logger,
		Stderr:     io.Discard,
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	defer func() {
		if err := agentRuntime.Close(); err != nil {
			log.Printf("close ACP agent: %v", err)
		}
	}()

	sessionService := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        "codex-demo",
		Agent:          agentRuntime,
		SessionService: sessionService,
	})
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}

	sess, err := sessionService.Create(ctx, &session.CreateRequest{
		AppName: "codex-demo",
		UserID:  "user-1",
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	prompt := genai.NewContentFromText("Explain this project structure", genai.RoleUser)
	for ev, err := range r.Run(ctx, "user-1", sess.Session.ID(), prompt, agent.RunConfig{}) {
		if err != nil {
			return fmt.Errorf("run turn: %w", err)
		}
		if ev.Content != nil {
			for _, part := range ev.Content.Parts {
				if part.Text != "" && !part.Thought {
					fmt.Print(part.Text)
				}
			}
		}
	}
	fmt.Println()
	return nil
}

