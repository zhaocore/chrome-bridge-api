package session

import "sync"

type Store struct {
	mu       sync.Mutex
	sessions map[string]*State
}

type State struct {
	TabID  int
	TabIDs []int
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]*State)}
}

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

func (s *Store) Update(action, name string, data any) {
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
	state.TabID = tabID
	for _, existing := range state.TabIDs {
		if existing == tabID {
			return
		}
	}
	state.TabIDs = append(state.TabIDs, tabID)
}

func (s *Store) Snapshot(name string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[name]
	if state == nil {
		return State{}
	}
	return State{TabID: state.TabID, TabIDs: append([]int(nil), state.TabIDs...)}
}

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
