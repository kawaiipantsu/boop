package web

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// readServerMessage reads and decodes one envelope with a bounded wait.
func readServerMessage(t *testing.T, conn *websocket.Conn) (ServerMessage, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		return ServerMessage{}, err
	}
	var msg ServerMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return ServerMessage{}, err
	}
	return msg, nil
}

// writeClientMessage sends one client envelope.
func writeClientMessage(t *testing.T, conn *websocket.Conn, msg ClientMessageEnvelope) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// dialEvents connects to the event stream with a same-origin handshake.
func dialEvents(t *testing.T, base string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	if opts == nil {
		opts = &websocket.DialOptions{}
	}
	if opts.HTTPHeader == nil {
		opts.HTTPHeader = http.Header{}
	}
	if opts.HTTPHeader.Get("Origin") == "" {
		opts.HTTPHeader.Set("Origin", base)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, wsURL(base), opts)
}

// TestEventStreamDeliversBusEvents is the §25 contract: a core event reaches
// every connected client unchanged, in the same vocabulary the TUI sees.
func TestEventStreamDeliversBusEvents(t *testing.T) {
	application := newTestApp(t)
	srv, base := newRunningServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config()
	})

	conn, _, err := dialEvents(t, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	hello, err := readServerMessage(t, conn)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if hello.Type != MsgHello {
		t.Fatalf("first message = %q, want %q", hello.Type, MsgHello)
	}

	// The bridge is registered at construction, but the client only counts
	// once its handler has run; wait for it rather than racing the publish.
	waitFor(t, func() bool { return srv.hub.count() == 1 })

	application.Bus.Publish(app.Event{
		Type:      app.EventModelToken,
		SessionID: "session-1",
		Payload:   map[string]any{"text": "hello"},
	})

	msg, err := readServerMessage(t, conn)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	if msg.Type != MsgEvent {
		t.Fatalf("type = %q, want %q", msg.Type, MsgEvent)
	}
	if msg.Event == nil {
		t.Fatal("no event payload")
	}
	if msg.Event.Type != app.EventModelToken {
		t.Errorf("event type = %q, want %q", msg.Event.Type, app.EventModelToken)
	}
	if msg.Event.SessionID != "session-1" {
		t.Errorf("session id = %q", msg.Event.SessionID)
	}
	if msg.Event.At.IsZero() {
		t.Error("the event has no timestamp")
	}
}

// TestEventFilter checks the per-connection subscription, which is how a view
// that only cares about approvals avoids a token firehose.
func TestEventFilter(t *testing.T) {
	application := newTestApp(t)
	srv, base := newRunningServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config()
	})

	conn, _, err := dialEvents(t, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	if _, err := readServerMessage(t, conn); err != nil {
		t.Fatalf("hello: %v", err)
	}
	waitFor(t, func() bool { return srv.hub.count() == 1 })

	data, _ := json.Marshal(subscribeData{Types: []app.EventType{app.EventToolRequested}})
	writeClientMessage(t, conn, ClientMessageEnvelope{Type: ClientSubscribe, ID: "sub-1", Data: data})

	ack, err := readServerMessage(t, conn)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if ack.Type != MsgAck || ack.ID != "sub-1" {
		t.Fatalf("ack = %+v", ack)
	}

	// The filtered-out event must not arrive; the subscribed one must.
	application.Bus.Publish(app.Event{Type: app.EventModelToken, Payload: "ignored"})
	application.Bus.Publish(app.Event{Type: app.EventToolRequested, Payload: "wanted"})

	msg, err := readServerMessage(t, conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Event == nil || msg.Event.Type != app.EventToolRequested {
		t.Fatalf("received %+v, want only the subscribed event type", msg.Event)
	}
}

// TestApprovalOverWebSocket: §24 prefers a WebSocket because approvals are
// interactive, so the answer must travel back over the same socket.
func TestApprovalOverWebSocket(t *testing.T) {
	broker := permissions.NewBroker()
	t.Cleanup(broker.Close)
	application := newTestApp(t)
	_, base := newRunningServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config()
		o.Broker = broker
	})

	conn, _, err := dialEvents(t, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	if _, err := readServerMessage(t, conn); err != nil {
		t.Fatalf("hello: %v", err)
	}

	answered := make(chan bool, 1)
	go func() {
		ok, err := broker.RequestDecision(context.Background(), permissions.Action{
			Category: permissions.CatFilesystemWrite,
			Risk:     permissions.RiskMedium,
			Tool:     "write",
			Summary:  "write main.go",
		}, permissions.Decision{Outcome: permissions.OutcomeConfirm, Reason: "writes require confirmation"})
		if err != nil {
			answered <- false
			return
		}
		answered <- ok
	}()

	// The pending approval is pushed to the client without being asked for.
	var pendingID string
	for pendingID == "" {
		msg, err := readServerMessage(t, conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if msg.Type != MsgApproval {
			continue
		}
		var ev permissions.ApprovalEvent
		raw, _ := json.Marshal(msg.Data)
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode approval event: %v", err)
		}
		if ev.Kind == permissions.ApprovalAdded {
			pendingID = ev.Approval.ID
		}
	}

	data, _ := json.Marshal(approvalRequest{ID: pendingID, Approved: true})
	writeClientMessage(t, conn, ClientMessageEnvelope{Type: ClientApproval, ID: "ap-1", Data: data})

	select {
	case ok := <-answered:
		if !ok {
			t.Error("the core saw a denial")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the approval was never resolved")
	}
}

// TestWebSocketOriginValidation is the §23 test that matters most: a browser
// will happily point a hostile page at a loopback socket, and the server is
// the only thing that can say no.
func TestWebSocketOriginValidation(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		origin  string
		// setOrigin false omits the header entirely.
		setOrigin bool
		wantOK    bool
	}{
		{name: "same origin", setOrigin: true, wantOK: true},
		{name: "no origin at all is refused"},
		{name: "hostile origin is refused", setOrigin: true, origin: "https://evil.example"},
		{
			name: "allowlisted origin is accepted", allowed: []string{"http://localhost:5173"},
			setOrigin: true, origin: "http://localhost:5173", wantOK: true,
		},
		{
			name: "an origin off the allowlist is refused", allowed: []string{"http://localhost:5173"},
			setOrigin: true, origin: "http://localhost:5174",
		},
		{name: "null origin is refused", setOrigin: true, origin: "null"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, base := newRunningServer(t, func(o *Options) {
				o.Config.Web.AllowedOrigins = tc.allowed
			})
			header := http.Header{}
			if tc.setOrigin {
				origin := tc.origin
				if origin == "" {
					origin = base
				}
				header.Set("Origin", origin)
			} else {
				// Suppress dialEvents' same-origin default.
				header.Set("Origin", "")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, resp, err := websocket.Dial(ctx, wsURL(base), &websocket.DialOptions{HTTPHeader: header})
			if conn != nil {
				defer conn.CloseNow()
			}
			if tc.wantOK {
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the upgrade succeeded, want it refused")
			}
			if resp != nil && resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

// TestWebSocketTokenAuth checks the two credential channels a browser can use
// on an upgrade, since it cannot set an Authorization header.
func TestWebSocketTokenAuth(t *testing.T) {
	const token = "ws-token-value"

	newServer := func(t *testing.T) string {
		cfg := config.Default()
		cfg.Web.Auth = config.AuthConfig{Enabled: true, TokenEnv: "BOOP_WEB_TOKEN"}
		_, base := newRunningServer(t, func(o *Options) {
			o.Config = cfg
			o.LookupEnv = envOf(map[string]string{"BOOP_WEB_TOKEN": token})
		})
		return base
	}

	t.Run("subprotocol carries the token", func(t *testing.T) {
		base := newServer(t)
		conn, _, err := dialEvents(t, base, &websocket.DialOptions{
			Subprotocols: []string{tokenSubprotocolPrefix + token},
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.CloseNow()
		if got := conn.Subprotocol(); got != tokenSubprotocolPrefix+token {
			t.Errorf("negotiated subprotocol = %q, want it echoed back", got)
		}
	})

	t.Run("query parameter carries the token", func(t *testing.T) {
		base := newServer(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(ctx, wsURL(base)+"?"+tokenQueryParam+"="+token,
			&websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{base}}})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conn.CloseNow()
	})

	t.Run("no token is refused", func(t *testing.T) {
		base := newServer(t)
		conn, resp, err := dialEvents(t, base, nil)
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil {
			t.Fatal("the upgrade succeeded without a token")
		}
		if resp != nil && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("a wrong token is refused", func(t *testing.T) {
		base := newServer(t)
		conn, resp, err := dialEvents(t, base, &websocket.DialOptions{
			Subprotocols: []string{tokenSubprotocolPrefix + "wrong"},
		})
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil {
			t.Fatal("the upgrade succeeded with a wrong token")
		}
		if resp != nil && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// TestSlowClientDoesNotStallTheBus is the load-bearing property of the fan-out
// (§25): the bus publishes on the model loop's goroutine, so a browser that
// stops reading must lose messages, never hold the runtime still.
func TestSlowClientDoesNotStallTheBus(t *testing.T) {
	bus := app.NewBus()
	h := newHub()

	// A client with no reader attached: its send buffer fills and stays full,
	// which is exactly the state a stalled browser tab puts us in.
	stalled := newClient(nil)
	if !h.add(stalled) {
		t.Fatal("hub refused the client")
	}
	cancel := bus.Subscribe(func(ev app.Event) { h.broadcastEvent(ev) })
	defer cancel()

	const events = sendBuffer + maxDropped + 100
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for i := 0; i < events; i++ {
			bus.Publish(app.Event{Type: app.EventModelToken, Payload: i})
		}
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		if elapsed > 5*time.Second {
			t.Fatalf("publishing %d events took %v; the bus was being throttled by the client", events, elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bus blocked on a client that stopped reading")
	}

	if got := stalled.dropped.Load(); got == 0 {
		t.Error("no messages were dropped; the buffer cannot have been exercised")
	}
	select {
	case <-stalled.kill:
	default:
		t.Error("a client that dropped past the limit was not disconnected")
	}
}

// TestSlowClientIsDisconnected drives the policy over a real connection: a
// peer that never reads is eventually closed rather than buffered forever.
func TestSlowClientIsDisconnected(t *testing.T) {
	application := newTestApp(t)
	srv, base := newRunningServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config()
	})

	conn, _, err := dialEvents(t, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	waitFor(t, func() bool { return srv.hub.count() == 1 })

	// Never read from conn. Publish far more than the buffer and the drop
	// allowance can absorb; publishing must stay fast throughout.
	start := time.Now()
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = 'x'
	}
	for i := 0; i < sendBuffer+maxDropped+1000; i++ {
		application.Bus.Publish(app.Event{Type: app.EventModelToken, Payload: string(payload)})
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("publishing took %v; the runtime was throttled by an unread socket", elapsed)
	}

	waitFor(t, func() bool { return srv.hub.count() == 0 })
}

// TestClientPingPong checks the application-level liveness round trip.
func TestClientPingPong(t *testing.T) {
	_, base := newRunningServer(t, nil)
	conn, _, err := dialEvents(t, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	if _, err := readServerMessage(t, conn); err != nil {
		t.Fatalf("hello: %v", err)
	}

	writeClientMessage(t, conn, ClientMessageEnvelope{Type: ClientPing, ID: "p1"})
	msg, err := readServerMessage(t, conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Type != MsgPong || msg.ID != "p1" {
		t.Fatalf("got %+v, want a pong echoing p1", msg)
	}
}

// TestUnknownClientMessage keeps the socket alive after a client mistake.
func TestUnknownClientMessage(t *testing.T) {
	_, base := newRunningServer(t, nil)
	conn, _, err := dialEvents(t, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	if _, err := readServerMessage(t, conn); err != nil {
		t.Fatalf("hello: %v", err)
	}

	writeClientMessage(t, conn, ClientMessageEnvelope{Type: "teleport", ID: "x1"})
	msg, err := readServerMessage(t, conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Type != MsgError || msg.Error == nil || msg.Error.Code != codeBadRequest {
		t.Fatalf("got %+v, want a bad_request error", msg)
	}

	// The connection survives, so one bad frame does not cost the session.
	writeClientMessage(t, conn, ClientMessageEnvelope{Type: ClientPing, ID: "x2"})
	if msg, err = readServerMessage(t, conn); err != nil || msg.Type != MsgPong {
		t.Fatalf("got %+v, %v; want a pong on the surviving connection", msg, err)
	}
}

// TestHelloCarriesPendingApprovals: a client that connects mid-run must not
// have to wait for the next event to learn there is a decision waiting (§50).
func TestHelloCarriesPendingApprovals(t *testing.T) {
	broker := permissions.NewBroker()
	t.Cleanup(broker.Close)
	_, base := newRunningServer(t, func(o *Options) { o.Broker = broker })

	go func() {
		_, _ = broker.Request(context.Background(), permissions.Action{
			Category: permissions.CatShellExecute,
			Tool:     "run",
			Summary:  "run `ls`",
		})
	}()
	waitFor(t, func() bool { return len(broker.Pending()) == 1 })

	conn, _, err := dialEvents(t, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	msg, err := readServerMessage(t, conn)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	raw, _ := json.Marshal(msg.Data)
	var hello helloData
	if err := json.Unmarshal(raw, &hello); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if hello.Protocol != ProtocolVersion {
		t.Errorf("protocol = %d, want %d", hello.Protocol, ProtocolVersion)
	}
	if len(hello.PendingApprovals) != 1 {
		t.Fatalf("pending approvals = %+v, want the queued one", hello.PendingApprovals)
	}
}

// waitFor polls a condition with a bounded deadline.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition was never satisfied")
}
