// Package bridge manages the WebSocket connection to the Chrome extension and
// routes tool calls from the HTTP API to the extension's debugger-backed tools.
//
// The extension is the single source of browser state: the daemon forwards a
// tool_call over the WebSocket and waits for the matching tool_result keyed by
// request ID. Connections are isolated by the extension instance ID.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	// ErrExtensionNotConnected is returned when a tool call is made with no
	// extension WebSocket connected.
	ErrExtensionNotConnected = errors.New("extension not connected")
	// ErrExtensionDisconnected is returned to in-flight callers when the
	// extension WebSocket drops mid-call.
	ErrExtensionDisconnected = errors.New("extension disconnected")
	// ErrExtensionAmbiguous is returned when more than one extension is connected
	// and the caller did not select an instance.
	ErrExtensionAmbiguous = errors.New("multiple extensions connected; instance_id is required")
)

const legacyInstanceID = "legacy"

// Logger is the minimal logging interface used by the bridge.
type Logger interface {
	Printf(format string, v ...any)
}

// noopLogger discards all log output.
type noopLogger struct{}

func (noopLogger) Printf(string, ...any) {}

// Status describes the current extension connection, surfaced via the HTTP
// /status endpoint.
type Status struct {
	Connected        bool             `json:"extension_connected"`
	InstanceID       string           `json:"instance_id"`
	ExtensionID      string           `json:"extension_id"`
	ExtensionName    string           `json:"extension_name,omitempty"`
	ExtensionVersion string           `json:"extension_version"`
	Instances        []InstanceStatus `json:"extensions"`
}

// InstanceStatus describes one connected extension instance.
type InstanceStatus struct {
	InstanceID       string `json:"instance_id"`
	ExtensionID      string `json:"extension_id"`
	ExtensionName    string `json:"extension_name,omitempty"`
	ExtensionVersion string `json:"extension_version"`
}

// client owns one extension WebSocket and serializes writes to it.
type client struct {
	conn             *websocket.Conn
	writeMu          sync.Mutex
	instanceID       string
	extensionID      string
	extensionName    string
	extensionVersion string
}

// Manager owns the extension WebSocket and tracks in-flight tool calls by
// request ID. It is safe for concurrent use.
type Manager struct {
	version string
	logger  Logger

	upgrader websocket.Upgrader
	nextID   atomic.Uint64

	mu      sync.Mutex
	clients map[string]*client
	// pending maps daemon request IDs to the HTTP handler waiting for the
	// matching tool_result from the extension.
	pending map[string]pendingCall
}

type pendingCall struct {
	client   *client
	resultCh chan callResult
}

// callResult carries a tool call's outcome back to its waiting caller.
type callResult struct {
	data any
	err  error
}

// Message is the envelope exchanged with the extension over the WebSocket.
type Message struct {
	Type                string         `json:"type"`
	RequestID           string         `json:"requestId,omitempty"`
	ResponseToRequestID string         `json:"responseToRequestId,omitempty"`
	Payload             map[string]any `json:"payload,omitempty"`
}

// NewManager returns a Manager bound to the given daemon version and logger.
// A nil logger is replaced with a no-op.
func NewManager(version string, logger Logger) *Manager {
	if logger == nil {
		logger = noopLogger{}
	}
	return &Manager{
		version: version,
		logger:  logger,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		},
		clients: make(map[string]*client),
		pending: make(map[string]pendingCall),
	}
}

// Status returns a snapshot of the extension connection.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{
		Connected: len(m.clients) > 0,
		Instances: make([]InstanceStatus, 0, len(m.clients)),
	}
	instanceIDs := make([]string, 0, len(m.clients))
	for instanceID := range m.clients {
		instanceIDs = append(instanceIDs, instanceID)
	}
	sort.Strings(instanceIDs)
	for _, instanceID := range instanceIDs {
		connectedClient := m.clients[instanceID]
		instance := InstanceStatus{
			InstanceID:       connectedClient.instanceID,
			ExtensionID:      connectedClient.extensionID,
			ExtensionName:    connectedClient.extensionName,
			ExtensionVersion: connectedClient.extensionVersion,
		}
		status.Instances = append(status.Instances, instance)
		if status.InstanceID == "" {
			status.InstanceID = instance.InstanceID
			status.ExtensionID = instance.ExtensionID
			status.ExtensionName = instance.ExtensionName
			status.ExtensionVersion = instance.ExtensionVersion
		}
	}
	return status
}

// ServeWS upgrades the HTTP request to the extension WebSocket and runs its
// read loop to completion. The hello message registers the instance identity.
func (m *Manager) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		m.logger.Printf("websocket upgrade failed: %v", err)
		return
	}

	m.logger.Printf("extension websocket connected")
	m.readLoop(&client{conn: conn})
}

// Call sends a tool_call to the extension and blocks until the matching
// tool_result arrives or ctx is cancelled. It returns the selected instance ID.
func (m *Manager) Call(ctx context.Context, instanceID string, name string, args map[string]any) (any, string, error) {
	requestID := fmt.Sprintf("req-%d", m.nextID.Add(1))
	resultCh := make(chan callResult, 1)
	started := time.Now()

	m.mu.Lock()
	connectedClient, err := m.selectClient(instanceID)
	if err != nil {
		m.mu.Unlock()
		return nil, "", err
	}
	m.pending[requestID] = pendingCall{client: connectedClient, resultCh: resultCh}
	m.mu.Unlock()

	// The extension protocol is request/response over one WebSocket: send a
	// tool_call now, then wait until handleToolResult resolves this request ID.
	message := Message{
		Type:      "tool_call",
		RequestID: requestID,
		Payload: map[string]any{
			"name": name,
			"args": args,
		},
	}
	if err := m.writeJSON(connectedClient, message); err != nil {
		m.removePending(requestID)
		m.logger.Printf("tool_call write_error request_id=%s instance_id=%s action=%s error=%q", requestID, connectedClient.instanceID, name, err.Error())
		return nil, connectedClient.instanceID, err
	}
	m.logger.Printf("tool_call sent request_id=%s instance_id=%s action=%s", requestID, connectedClient.instanceID, name)

	select {
	case result := <-resultCh:
		if result.err != nil {
			m.logger.Printf("tool_call result_error request_id=%s action=%s duration_ms=%d error=%q", requestID, name, time.Since(started).Milliseconds(), result.err.Error())
		} else {
			m.logger.Printf("tool_call result_ok request_id=%s action=%s duration_ms=%d", requestID, name, time.Since(started).Milliseconds())
		}
		return result.data, connectedClient.instanceID, result.err
	case <-ctx.Done():
		m.removePending(requestID)
		m.logger.Printf("tool_call timeout request_id=%s action=%s duration_ms=%d error=%q", requestID, name, time.Since(started).Milliseconds(), ctx.Err().Error())
		return nil, connectedClient.instanceID, ctx.Err()
	}
}

// selectClient resolves an explicit instance or the only connected instance.
// m.mu must be held by the caller.
func (m *Manager) selectClient(instanceID string) (*client, error) {
	if instanceID != "" {
		connectedClient := m.clients[instanceID]
		if connectedClient == nil {
			return nil, fmt.Errorf("%w: instance %q", ErrExtensionNotConnected, instanceID)
		}
		return connectedClient, nil
	}
	if len(m.clients) == 0 {
		return nil, ErrExtensionNotConnected
	}
	if len(m.clients) > 1 {
		return nil, ErrExtensionAmbiguous
	}
	for _, connectedClient := range m.clients {
		return connectedClient, nil
	}
	return nil, ErrExtensionNotConnected
}

// Ping sends a low-level ping message to confirm the WebSocket is alive.
func (m *Manager) Ping(ctx context.Context) error {
	requestID := fmt.Sprintf("ping-%d", time.Now().UnixNano())
	m.mu.Lock()
	connectedClient, err := m.selectClient("")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return m.writeJSON(connectedClient, Message{Type: "ping", RequestID: requestID})
}

// readLoop reads WebSocket messages until the connection errors out, then
// disconnects.
func (m *Manager) readLoop(connectedClient *client) {
	defer m.disconnect(connectedClient)
	for {
		var msg Message
		if err := connectedClient.conn.ReadJSON(&msg); err != nil {
			m.logger.Printf("websocket read ended: %v", err)
			return
		}
		m.handleMessage(connectedClient, msg)
	}
}

// handleMessage dispatches an inbound message by type.
func (m *Manager) handleMessage(connectedClient *client, msg Message) {
	switch msg.Type {
	case "hello":
		m.handleHello(connectedClient, msg)
	case "pong":
		return
	case "tool_result":
		m.handleToolResult(connectedClient, msg)
	default:
		m.logger.Printf("unhandled websocket message type: %s", msg.Type)
	}
}

// handleHello records the extension's identity from its hello payload and acks it.
func (m *Manager) handleHello(connectedClient *client, msg Message) {
	name, _ := msg.Payload["extensionName"].(string)
	version, _ := msg.Payload["extensionVersion"].(string)
	id, _ := msg.Payload["extensionId"].(string)
	instanceID, _ := msg.Payload["instanceId"].(string)
	if instanceID == "" {
		instanceID = legacyInstanceID
	}

	m.mu.Lock()
	if connectedClient.instanceID != "" {
		registeredInstanceID := connectedClient.instanceID
		m.mu.Unlock()
		m.logger.Printf("duplicate hello ignored registered_instance_id=%q requested_instance_id=%q", registeredInstanceID, instanceID)
		m.sendHelloAck(connectedClient, msg.RequestID, registeredInstanceID)
		return
	}
	previous := m.clients[instanceID]
	connectedClient.instanceID = instanceID
	connectedClient.extensionName = name
	connectedClient.extensionVersion = version
	connectedClient.extensionID = id
	m.clients[instanceID] = connectedClient
	m.mu.Unlock()
	if previous != nil && previous != connectedClient {
		_ = previous.conn.Close()
	}
	m.logger.Printf("extension hello instance_id=%q name=%q version=%q id=%q", instanceID, name, version, id)
	m.sendHelloAck(connectedClient, msg.RequestID, instanceID)
}

// sendHelloAck 返回当前连接实际登记的扩展实例标识。
func (m *Manager) sendHelloAck(connectedClient *client, requestID string, instanceID string) {
	ack := Message{
		Type:      "hello_ack",
		RequestID: requestID,
		Payload: map[string]any{
			"version":    m.version,
			"instanceId": instanceID,
		},
	}
	if err := m.writeJSON(connectedClient, ack); err != nil {
		m.logger.Printf("hello_ack failed: %v", err)
	}
}

// handleToolResult resolves the in-flight call waiting on the result's
// responseToRequestId, if any.
func (m *Manager) handleToolResult(connectedClient *client, msg Message) {
	requestID := msg.ResponseToRequestID
	if requestID == "" {
		m.logger.Printf("tool_result ignored missing_response_to_request_id")
		return
	}

	m.mu.Lock()
	pending, ok := m.pending[requestID]
	if !ok {
		m.mu.Unlock()
		m.logger.Printf("tool_result ignored unknown_request_id=%s", requestID)
		return
	}
	if pending.client != connectedClient {
		m.mu.Unlock()
		m.logger.Printf("tool_result ignored wrong_instance request_id=%s instance_id=%s", requestID, connectedClient.instanceID)
		return
	}
	delete(m.pending, requestID)
	m.mu.Unlock()
	if errText, ok := msg.Payload["error"].(string); ok && errText != "" {
		pending.resultCh <- callResult{err: errors.New(errText)}
		return
	}
	pending.resultCh <- callResult{data: msg.Payload["data"]}
}

// writeJSON serializes concurrent writes to the WebSocket, which is required by
// gorilla/websocket.
func (m *Manager) writeJSON(connectedClient *client, value any) error {
	connectedClient.writeMu.Lock()
	defer connectedClient.writeMu.Unlock()
	return connectedClient.conn.WriteJSON(value)
}

// removePending drops a pending request ID without resolving its caller.
func (m *Manager) removePending(requestID string) {
	m.mu.Lock()
	delete(m.pending, requestID)
	m.mu.Unlock()
}

// disconnect closes conn and, if it is the active connection, clears extension
// state and fails all in-flight callers so HTTP requests do not wait out their
// timeouts.
func (m *Manager) disconnect(connectedClient *client) {
	_ = connectedClient.conn.Close()

	m.mu.Lock()
	if connectedClient.instanceID != "" && m.clients[connectedClient.instanceID] == connectedClient {
		delete(m.clients, connectedClient.instanceID)
	}
	pending := make([]chan callResult, 0)
	for requestID, call := range m.pending {
		if call.client == connectedClient {
			pending = append(pending, call.resultCh)
			delete(m.pending, requestID)
		}
	}
	m.mu.Unlock()

	// Unblock all HTTP callers; otherwise /command requests would wait until
	// their individual timeouts after the browser extension disconnects.
	for _, resultCh := range pending {
		resultCh <- callResult{err: ErrExtensionDisconnected}
	}
	m.logger.Printf("extension websocket disconnected instance_id=%q", connectedClient.instanceID)
}
