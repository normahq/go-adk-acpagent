package acpagent

import (
	"context"
	"fmt"
	"os"
	"strings"
)

func ExampleNewClient() {
	ctx := context.Background()
	previous := os.Getenv("GO_WANT_ACP_HELPER")
	_ = os.Setenv("GO_WANT_ACP_HELPER", "1")
	defer func() {
		if previous == "" {
			_ = os.Unsetenv("GO_WANT_ACP_HELPER")
			return
		}
		_ = os.Setenv("GO_WANT_ACP_HELPER", previous)
	}()

	workingDir, err := os.MkdirTemp("", "runtime-acpagent-example-*")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = os.RemoveAll(workingDir) }()

	client, err := NewClient(ctx, ClientConfig{
		Command: []string{os.Args[0], "-test.run=TestACPHelperProcess", "--"},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Println(err)
		}
	}()

	if _, err := client.Initialize(ctx); err != nil {
		fmt.Println(err)
		return
	}
	sessionResp, err := client.NewSession(ctx, workingDir, nil)
	if err != nil {
		fmt.Println(err)
		return
	}

	updates, results, err := client.Prompt(ctx, string(sessionResp.SessionId), "hello")
	if err != nil {
		fmt.Println(err)
		return
	}
	var output strings.Builder
	for updates != nil || results != nil {
		select {
		case update, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			ev, ok := mapACPUpdateToEvent(ctx, newLogger(nil, ""), "example", update)
			if ok {
				output.WriteString(extractPromptText(ev.Content))
			}
		case result, ok := <-results:
			if !ok {
				results = nil
				continue
			}
			if result.Err != nil {
				fmt.Println(result.Err)
				return
			}
		}
	}

	fmt.Println(output.String())
	// Output:
	// session-1:hello
}
