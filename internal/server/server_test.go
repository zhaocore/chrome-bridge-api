package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStatusAndConnections(t *testing.T) {
	s := New(Config{Version: "test", Host: "127.0.0.1", Port: 10089}, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code %d body %s", rec.Code, rec.Body.String())
	}
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["running"] != true || status["extension_connected"] != false {
		t.Fatalf("unexpected status %#v", status)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/connections", nil)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("connections code %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ws://127.0.0.1:10089/ws") {
		t.Fatalf("unexpected connection response %s", rec.Body.String())
	}
}

func TestCommandRequiresExtension(t *testing.T) {
	s := New(Config{Version: "test"}, nil, nil)
	body := bytes.NewBufferString(`{"action":"snapshot","args":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/command", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestCommandOverWebSocketAndSessionInjection(t *testing.T) {
	s := New(Config{Version: "test"}, nil, nil)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"type": "hello",
		"payload": map[string]any{
			"extensionName":    "chrome-bridge",
			"extensionVersion": "0.1.0",
		},
	}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	if ack["type"] != "hello_ack" {
		t.Fatalf("unexpected ack %#v", ack)
	}

	go func() {
		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			payload := msg["payload"].(map[string]any)
			name := payload["name"].(string)
			args := payload["args"].(map[string]any)
			var data map[string]any
			switch name {
			case "navigate":
				data = map[string]any{"success": true, "url": args["url"], "tabId": 77}
			case "click":
				if args["_tabId"] != float64(77) {
					data = map[string]any{"badTab": args["_tabId"]}
				} else {
					data = map[string]any{"success": true, "tag": "BUTTON"}
				}
			default:
				data = map[string]any{"success": true}
			}
			_ = conn.WriteJSON(map[string]any{
				"type":                "tool_result",
				"responseToRequestId": msg["requestId"],
				"payload":             map[string]any{"data": data},
			})
		}
	}()

	postCommand(t, ts.URL, `{"action":"navigate","session":"task","args":{"url":"https://example.com","newTab":true}}`)
	result := postCommand(t, ts.URL, `{"action":"click","session":"task","args":{"selector":"#go"}}`)
	data := result["data"].(map[string]any)
	if data["success"] != true {
		t.Fatalf("expected success, got %#v", data)
	}
}

func TestCommandsStayBoundToExtensionInstance(t *testing.T) {
	s := New(Config{Version: "test"}, nil, nil)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	first := connectExtension(t, ts.URL, "instance-a")
	defer first.Close()
	second := connectExtension(t, ts.URL, "instance-b")
	defer second.Close()
	serveExtension(first, "instance-a", 101)
	serveExtension(second, "instance-b", 202)

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	extensions := status["extensions"].([]any)
	if len(extensions) != 2 {
		t.Fatalf("expected two connected extensions, got %#v", status)
	}

	statusCode, ambiguous := postCommandStatus(
		t,
		ts.URL,
		`{"action":"navigate","session":"ambiguous","args":{"url":"https://example.com","newTab":true}}`,
	)
	if statusCode != http.StatusConflict || !strings.Contains(ambiguous["error"].(string), "instance_id is required") {
		t.Fatalf("expected ambiguous instance conflict, code %d body %#v", statusCode, ambiguous)
	}

	postCommand(t, ts.URL, `{"action":"navigate","session":"task-a","instance_id":"instance-a","args":{"url":"https://example.com/a","newTab":true}}`)
	postCommand(t, ts.URL, `{"action":"navigate","session":"task-b","instance_id":"instance-b","args":{"url":"https://example.com/b","newTab":true}}`)

	firstResult := postCommand(t, ts.URL, `{"action":"click","session":"task-a","args":{"selector":"#go"}}`)
	firstData := firstResult["data"].(map[string]any)
	if firstData["instanceId"] != "instance-a" || firstData["tabId"] != float64(101) {
		t.Fatalf("first session crossed instances: %#v", firstData)
	}

	secondResult := postCommand(t, ts.URL, `{"action":"click","session":"task-b","args":{"selector":"#go"}}`)
	secondData := secondResult["data"].(map[string]any)
	if secondData["instanceId"] != "instance-b" || secondData["tabId"] != float64(202) {
		t.Fatalf("second session crossed instances: %#v", secondData)
	}

	statusCode, conflict := postCommandStatus(
		t,
		ts.URL,
		`{"action":"click","session":"task-a","instance_id":"instance-b","args":{"selector":"#go"}}`,
	)
	if statusCode != http.StatusConflict || !strings.Contains(conflict["error"].(string), "is bound to extension instance") {
		t.Fatalf("expected session binding conflict, code %d body %#v", statusCode, conflict)
	}
}

func TestConcurrentFirstCommandsCannotBindOneSessionToDifferentInstances(t *testing.T) {
	s := New(Config{Version: "test"}, nil, nil)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	first := connectExtension(t, ts.URL, "instance-a")
	defer first.Close()
	second := connectExtension(t, ts.URL, "instance-b")
	defer second.Close()

	firstCallReceived := make(chan map[string]any, 1)
	releaseFirstCall := make(chan struct{})
	go func() {
		var msg map[string]any
		if err := first.ReadJSON(&msg); err != nil {
			return
		}
		firstCallReceived <- msg
		<-releaseFirstCall
		_ = first.WriteJSON(map[string]any{
			"type":                "tool_result",
			"responseToRequestId": msg["requestId"],
			"payload":             map[string]any{"data": map[string]any{"success": true, "tabId": 101}},
		})
	}()
	serveExtension(second, "instance-b", 202)

	type commandResponse struct {
		statusCode int
		body       map[string]any
	}
	firstResponse := make(chan commandResponse, 1)
	go func() {
		statusCode, body := postCommandStatus(t, ts.URL,
			`{"action":"navigate","session":"shared","instance_id":"instance-a","args":{"url":"https://example.com/a","newTab":true}}`)
		firstResponse <- commandResponse{statusCode: statusCode, body: body}
	}()
	select {
	case <-firstCallReceived:
	case <-time.After(time.Second):
		t.Fatal("first command did not reach extension")
	}

	secondResponse := make(chan commandResponse, 1)
	go func() {
		statusCode, body := postCommandStatus(t, ts.URL,
			`{"action":"navigate","session":"shared","instance_id":"instance-b","args":{"url":"https://example.com/b","newTab":true}}`)
		secondResponse <- commandResponse{statusCode: statusCode, body: body}
	}()

	close(releaseFirstCall)
	firstResult := <-firstResponse
	secondResult := <-secondResponse
	if firstResult.statusCode != http.StatusOK {
		t.Fatalf("first command failed: %#v", firstResult)
	}
	if secondResult.statusCode != http.StatusConflict || !strings.Contains(secondResult.body["error"].(string), "is bound to extension instance") {
		t.Fatalf("expected second command instance conflict, got %#v", secondResult)
	}

	state := s.sessions.Snapshot("shared")
	if state.InstanceID != "instance-a" || state.TabID != 101 || len(state.TabIDs) != 1 {
		t.Fatalf("unexpected session state %#v", state)
	}
}

func TestQueuedSessionCommandHonorsTimeout(t *testing.T) {
	s := New(Config{Version: "test"}, nil, nil)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	conn := connectExtension(t, ts.URL, "instance-a")
	defer conn.Close()
	firstCallReceived := make(chan map[string]any, 1)
	releaseFirstCall := make(chan struct{})
	go func() {
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		firstCallReceived <- msg
		<-releaseFirstCall
		_ = conn.WriteJSON(map[string]any{
			"type":                "tool_result",
			"responseToRequestId": msg["requestId"],
			"payload":             map[string]any{"data": map[string]any{"success": true, "tabId": 101}},
		})
	}()

	firstResponse := make(chan int, 1)
	go func() {
		statusCode, _ := postCommandStatus(t, ts.URL,
			`{"action":"navigate","session":"shared","instance_id":"instance-a","args":{"url":"https://example.com","newTab":true}}`)
		firstResponse <- statusCode
	}()
	select {
	case <-firstCallReceived:
	case <-time.After(time.Second):
		t.Fatal("first command did not reach extension")
	}

	statusCode, result := postCommandStatus(t, ts.URL,
		`{"action":"click","session":"shared","instance_id":"instance-a","args":{"selector":"#go"},"timeout_ms":20}`)
	if statusCode != http.StatusGatewayTimeout || result["error"] != context.DeadlineExceeded.Error() {
		t.Fatalf("expected queued command timeout, code %d body %#v", statusCode, result)
	}

	close(releaseFirstCall)
	if statusCode := <-firstResponse; statusCode != http.StatusOK {
		t.Fatalf("first command failed with status %d", statusCode)
	}
}

func TestDuplicateHelloDoesNotRegisterSecondInstance(t *testing.T) {
	s := New(Config{Version: "test"}, nil, nil)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	conn := connectExtension(t, ts.URL, "instance-a")
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{
		"type": "hello",
		"payload": map[string]any{
			"extensionName":    "chrome-bridge",
			"extensionVersion": "test",
			"instanceId":       "instance-b",
		},
	}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	if ack["payload"].(map[string]any)["instanceId"] != "instance-a" {
		t.Fatalf("duplicate hello changed registered instance: %#v", ack)
	}

	status := s.bridge.Status()
	if len(status.Instances) != 1 || status.Instances[0].InstanceID != "instance-a" {
		t.Fatalf("duplicate hello created unexpected instances: %#v", status)
	}
}

func TestReconnectReplacesSameExtensionInstance(t *testing.T) {
	s := New(Config{Version: "test"}, nil, nil)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	first := connectExtension(t, ts.URL, "instance-a")
	defer first.Close()
	second := connectExtension(t, ts.URL, "instance-a")
	defer second.Close()
	serveExtension(second, "instance-a", 303)

	result := postCommand(t, ts.URL, `{"action":"navigate","session":"task","instance_id":"instance-a","args":{"url":"https://example.com","newTab":true}}`)
	data := result["data"].(map[string]any)
	if data["instanceId"] != "instance-a" || data["tabId"] != float64(303) {
		t.Fatalf("reconnected instance did not receive command: %#v", data)
	}

	status := s.bridge.Status()
	if len(status.Instances) != 1 || status.Instances[0].InstanceID != "instance-a" {
		t.Fatalf("expected one replacement connection, got %#v", status)
	}
}

func TestScreenshotCommandWritesFile(t *testing.T) {
	s := New(Config{Version: "test"}, nil, nil)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "hello", "payload": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}

	go func() {
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type":                "tool_result",
			"responseToRequestId": msg["requestId"],
			"payload": map[string]any{"data": map[string]any{
				"format": "png",
				"data":   base64.StdEncoding.EncodeToString([]byte("image")),
			}},
		})
	}()

	path := t.TempDir() + "/shot.png"
	result := postCommand(t, ts.URL, `{"action":"screenshot","args":{"path":"`+path+`"}}`)
	data := result["data"].(map[string]any)
	if data["path"] != path || data["sizeBytes"] != float64(5) {
		t.Fatalf("unexpected screenshot result %#v", data)
	}
}

func TestCommandTimeout(t *testing.T) {
	s := New(Config{Version: "test"}, nil, nil)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "hello", "payload": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	go func() {
		var ignored map[string]any
		_ = conn.ReadJSON(&ignored)
		time.Sleep(50 * time.Millisecond)
	}()

	body := bytes.NewBufferString(`{"action":"snapshot","args":{},"timeout_ms":10}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/command", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected timeout status, got %d", resp.StatusCode)
	}
}

func postCommand(t *testing.T, baseURL, body string) map[string]any {
	t.Helper()
	statusCode, result := postCommandStatus(t, baseURL, body)
	if statusCode != http.StatusOK {
		t.Fatalf("code %d body %#v", statusCode, result)
	}
	return result
}

func postCommandStatus(t *testing.T, baseURL string, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/command", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, result
}

func connectExtension(t *testing.T, baseURL string, instanceID string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(baseURL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "hello",
		"payload": map[string]any{
			"extensionName":    "chrome-bridge",
			"extensionVersion": "test",
			"instanceId":       instanceID,
		},
	}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	if ack["type"] != "hello_ack" {
		t.Fatalf("unexpected ack %#v", ack)
	}
	return conn
}

func serveExtension(conn *websocket.Conn, instanceID string, tabID int) {
	go func() {
		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			payload := msg["payload"].(map[string]any)
			name := payload["name"].(string)
			args := payload["args"].(map[string]any)
			data := map[string]any{
				"success":    true,
				"instanceId": instanceID,
			}
			if name == "navigate" {
				data["tabId"] = tabID
			} else {
				data["tabId"] = args["_tabId"]
			}
			_ = conn.WriteJSON(map[string]any{
				"type":                "tool_result",
				"responseToRequestId": msg["requestId"],
				"payload":             map[string]any{"data": data},
			})
		}
	}()
}
