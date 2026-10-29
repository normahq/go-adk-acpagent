package acpagent

import (
	"context"
	"errors"
	"sync"

	"google.golang.org/adk/v2/session"
)

const maxModelRegistryEntries = 64

var (
	// ErrSessionBusy reports fail-fast contention on one exact ADK session.
	ErrSessionBusy = errors.New("acp adk session is busy")
	// ErrModelRegistryFull reports that no safe registry entry can be admitted.
	ErrModelRegistryFull = errors.New("acp model registry capacity unavailable")
)

type adkSessionKey struct {
	appName   string
	userID    string
	sessionID string
}

func sessionKey(sess session.Session) (adkSessionKey, error) {
	if sess == nil || sess.AppName() == "" || sess.UserID() == "" || sess.ID() == "" {
		return adkSessionKey{}, errors.New("adk session identity is required")
	}
	return adkSessionKey{appName: sess.AppName(), userID: sess.UserID(), sessionID: sess.ID()}, nil
}

type modelRegistryEntry struct {
	entryToken      uint64
	insertSequence  uint64
	attachmentToken uint64
	attachmentID    string
	refs            int
	busy            bool
	liveDesired     string
	control         *sessionControlState
	controlToken    uint64
}

type modelAdmission struct {
	registry *modelRegistry
	key      adkSessionKey
	entry    *modelRegistryEntry
	token    uint64
	once     sync.Once
}

func (a *modelAdmission) release() {
	if a == nil || a.registry == nil {
		return
	}
	a.once.Do(func() { a.registry.release(a) })
}

type modelRegistry struct {
	mu             sync.Mutex
	entries        map[adkSessionKey]*modelRegistryEntry
	nextEntryToken uint64
	nextSequence   uint64
	closed         bool
}

func newModelRegistry() *modelRegistry {
	return &modelRegistry{entries: make(map[adkSessionKey]*modelRegistryEntry)}
}

func (r *modelRegistry) acquire(ctx context.Context, sess session.Session) (*modelAdmission, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := sessionKey(sess)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, ErrClientClosed
	}
	entry := r.entries[key]
	if entry == nil {
		if len(r.entries) >= maxModelRegistryEntries && !r.evictOneLocked() {
			return nil, ErrModelRegistryFull
		}
		r.nextEntryToken++
		r.nextSequence++
		entry = &modelRegistryEntry{entryToken: r.nextEntryToken, insertSequence: r.nextSequence, attachmentToken: 1}
		r.entries[key] = entry
	}
	if entry.busy {
		return nil, ErrSessionBusy
	}
	entry.busy = true
	entry.refs++
	return &modelAdmission{registry: r, key: key, entry: entry, token: entry.entryToken}, nil
}

func (r *modelRegistry) release(a *modelAdmission) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[a.key]
	if entry != a.entry || entry.entryToken != a.token {
		return
	}
	if entry.refs > 0 {
		entry.refs--
	}
	entry.busy = false
	r.maybeFinalizeLocked(a.key, entry)
}

func (r *modelRegistry) maybeFinalizeLocked(key adkSessionKey, entry *modelRegistryEntry) {
	if entry.refs != 0 || entry.busy {
		return
	}
	if r.closed {
		delete(r.entries, key)
	}
}

func (r *modelRegistry) evictOneLocked() bool {
	var oldestKey adkSessionKey
	var oldest *modelRegistryEntry
	for key, entry := range r.entries {
		if entry.refs == 0 && !entry.busy && entry.liveDesired == "" && (oldest == nil || entry.insertSequence < oldest.insertSequence) {
			oldestKey = key
			oldest = entry
		}
	}
	if oldest == nil {
		return false
	}
	delete(r.entries, oldestKey)
	return true
}

func (r *modelRegistry) close() {
	r.mu.Lock()
	r.closed = true
	for key, entry := range r.entries {
		r.maybeFinalizeLocked(key, entry)
	}
	r.mu.Unlock()
}

func (a *modelAdmission) desired(construction string) string {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	if a.entry.liveDesired != "" {
		return a.entry.liveDesired
	}
	return construction
}

func (a *modelAdmission) replaceAttachment(remoteID string) uint64 {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	// The process-local attachment identity supports target-aware retry without
	// becoming durable ADK session state. Only an authoritative successful
	// attach/new replaces the current attachment.
	a.entry.attachmentToken++
	a.entry.attachmentID = remoteID
	a.entry.control = nil
	a.entry.controlToken = 0
	return a.entry.attachmentToken
}

func (a *modelAdmission) invalidateAttachment() uint64 {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	a.entry.attachmentToken++
	a.entry.attachmentID = ""
	a.entry.control = nil
	a.entry.controlToken = 0
	return a.entry.attachmentToken
}

func (a *modelAdmission) attachedRemoteID() string {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	return a.entry.attachmentID
}

func (a *modelAdmission) currentAttachmentToken() uint64 {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	return a.entry.attachmentToken
}

func (a *modelAdmission) confirmCurrentAttachment(control *sessionControlState, attachment uint64) bool {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	if attachment != a.entry.attachmentToken {
		return false
	}
	a.entry.control = control
	a.entry.controlToken = attachment
	return true
}

func (a *modelAdmission) observeCurrent(control *sessionControlState) {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	a.entry.control = control
	a.entry.controlToken = a.entry.attachmentToken
}

func (a *modelAdmission) observed() *sessionControlState {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	if a.entry.controlToken != a.entry.attachmentToken {
		return nil
	}
	return a.entry.control
}

func (a *modelAdmission) confirm(model, construction string, attachment uint64) bool {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	if attachment != a.entry.attachmentToken {
		return false
	}
	if model == construction {
		a.entry.liveDesired = ""
	} else {
		a.entry.liveDesired = model
	}
	return true
}

func (a *modelAdmission) invalidateVerification() {
	a.registry.mu.Lock()
	a.entry.control = nil
	a.entry.controlToken = 0
	a.registry.mu.Unlock()
}
