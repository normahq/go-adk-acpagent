package acpagent

import (
	"iter"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type readonlyInvocationContext struct {
	invocation adkagent.InvocationContext
}

func (c readonlyInvocationContext) Deadline() (time.Time, bool) {
	if c.invocation == nil {
		return time.Time{}, false
	}
	return c.invocation.Deadline()
}

func (c readonlyInvocationContext) Done() <-chan struct{} {
	if c.invocation == nil {
		return nil
	}
	return c.invocation.Done()
}

func (c readonlyInvocationContext) Err() error {
	if c.invocation == nil {
		return nil
	}
	return c.invocation.Err()
}

func (c readonlyInvocationContext) Value(key any) any {
	if c.invocation == nil {
		return nil
	}
	return c.invocation.Value(key)
}

func (c readonlyInvocationContext) UserContent() *genai.Content {
	if c.invocation == nil {
		return nil
	}
	return c.invocation.UserContent()
}

func (c readonlyInvocationContext) InvocationID() string {
	if c.invocation == nil {
		return ""
	}
	return c.invocation.InvocationID()
}

func (c readonlyInvocationContext) AgentName() string {
	if c.invocation == nil || c.invocation.Agent() == nil {
		return ""
	}
	return c.invocation.Agent().Name()
}

func (c readonlyInvocationContext) ReadonlyState() session.ReadonlyState {
	if c.invocation == nil || c.invocation.Session() == nil {
		return emptyReadonlyState{}
	}
	return c.invocation.Session().State()
}

func (c readonlyInvocationContext) UserID() string {
	if c.invocation == nil || c.invocation.Session() == nil {
		return ""
	}
	return c.invocation.Session().UserID()
}

func (c readonlyInvocationContext) AppName() string {
	if c.invocation == nil || c.invocation.Session() == nil {
		return ""
	}
	return c.invocation.Session().AppName()
}

func (c readonlyInvocationContext) SessionID() string {
	if c.invocation == nil || c.invocation.Session() == nil {
		return ""
	}
	return c.invocation.Session().ID()
}

func (c readonlyInvocationContext) Branch() string {
	if c.invocation == nil {
		return ""
	}
	return c.invocation.Branch()
}

type emptyReadonlyState struct{}

func (emptyReadonlyState) Get(string) (any, error) {
	return nil, session.ErrStateKeyNotExist
}

func (emptyReadonlyState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {}
}
