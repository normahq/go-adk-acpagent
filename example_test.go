package acpagent

import (
	"context"
	"fmt"
	"os"
)

func ExampleNewClient() {
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

	client, err := NewClient(context.Background(), ClientConfig{
		Command: []string{os.Args[0], "-test.run=TestACPHelperProcess", "--"},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		fmt.Println(err)
		return
	}
	sessionResp, err := client.NewSession(context.Background(), workingDir, nil)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(string(sessionResp.SessionId) != "")
	// Output:
	// true
}
