// Package session tracks the tab IDs the daemon has learned for a named agent
// session so subsequent tool calls target the right tab without the agent
// re-stating it. The extension owns the browser; this package only remembers
// the tab IDs returned by prior navigate/find_tab calls.
package session

import (
	"context"
	"fmt"
	"sync"
)

// Store maps session names to their tab state. It is safe for concurrent use.
type Store struct {
	mu           sync.Mutex
	sessions     map[string]*State
	sessionLocks map[string]chan struct{}
}

// State is the per-session tab tracking data injected into tool calls.
type State struct {
	InstanceID string
	TabID      int
	TabIDs     []int
}

// NewStore returns an empty session store.
func NewStore() *Store {
	return &Store{
		sessions:     make(map[string]*State),
		sessionLocks: make(map[string]chan struct{}),
	}
}

// LockSession 串行化同一会话的完整命令流程，并在请求超时或取消时停止等待。
func (s *Store) LockSession(ctx context.Context, name string) (func(), error) {
	if name == "" {
		return func() {}, nil
	}

	s.mu.Lock()
	sessionLock := s.sessionLocks[name]
	if sessionLock == nil {
		sessionLock = make(chan struct{}, 1)
		sessionLock <- struct{}{}
		s.sessionLocks[name] = sessionLock
	}
	s.mu.Unlock()

	select {
	case <-sessionLock:
		return func() { sessionLock <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Prepare clones args and injects the session name and any learned tab IDs
// relevant to action. With no session name it returns args unchanged.
func (s *Store) Prepare(action string, args map[string]any, name string) map[string]any {
	prepared := make(map[string]any, len(args)+2)
	for key, value := range args {
		prepared[key] = value
	}
	if name == "" {
		return prepared
	}

	prepared["_session"] = name
	s.mu.Lock()
	state := s.sessions[name]
	if state != nil {
		// The extension owns browser state. The daemon only injects the tab IDs
		// it learned from prior navigate/find_tab calls in the same session.
		switch action {
		case "list_tabs", "close_session":
			if len(state.TabIDs) > 0 {
				prepared["_tabIds"] = append([]int(nil), state.TabIDs...)
			}
		case "navigate", "find_tab":
		default:
			if state.TabID != 0 {
				prepared["_tabId"] = state.TabID
			}
		}
	}
	s.mu.Unlock()
	return prepared
}

// Update records the tab ID returned by navigate or find_tab into the named
// session. Other actions are ignored.
func (s *Store) Update(action, name string, instanceID string, data any) {
	if name == "" || (action != "navigate" && action != "find_tab") {
		return
	}
	tabID, ok := extractTabID(data)
	if !ok || tabID == 0 {
		return
	}

	// Only navigation-like tools establish a session target tab.
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[name]
	if state == nil {
		state = &State{}
		s.sessions[name] = state
	}
	state.InstanceID = instanceID
	state.TabID = tabID
	for _, existing := range state.TabIDs {
		if existing == tabID {
			return
		}
	}
	state.TabIDs = append(state.TabIDs, tabID)
}

// ResolveInstance returns the instance bound to a session. An explicit
// conflicting instance is rejected so one session cannot cross browser state.
func (s *Store) ResolveInstance(name string, requestedInstanceID string) (string, error) {
	if name == "" {
		return requestedInstanceID, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[name]
	if state == nil || state.InstanceID == "" {
		return requestedInstanceID, nil
	}
	if requestedInstanceID != "" && requestedInstanceID != state.InstanceID {
		return "", fmt.Errorf(
			"session %q is bound to extension instance %q, got %q",
			name,
			state.InstanceID,
			requestedInstanceID,
		)
	}
	return state.InstanceID, nil
}

// Snapshot returns a copy of the named session's state, or the zero value if
// the session has no recorded state.
func (s *Store) Snapshot(name string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[name]
	if state == nil {
		return State{}
	}
	return State{
		InstanceID: state.InstanceID,
		TabID:      state.TabID,
		TabIDs:     append([]int(nil), state.TabIDs...),
	}
}

// extractTabID pulls the tabId field from a tool result payload, tolerating the
// numeric widths JSON unmarshaling may produce.
func extractTabID(data any) (int, bool) {
	m, ok := data.(map[string]any)
	if !ok {
		return 0, false
	}
	switch value := m["tabId"].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	case float32:
		return int(value), true
	default:
		return 0, false
	}
}
