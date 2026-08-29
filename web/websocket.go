package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// EventsPath is the WebSocket endpoint carrying the core event stream (§25).
//
// §24 prefers a WebSocket over server-sent events precisely because approvals
// are interactive: the same socket that reports "approval requested" carries
// the answer back.
const EventsPath = "/api/events"

// Protocol version of the WebSocket envelope. It is announced in the hello
// message so a frontend can refuse a server it does not understand.
const ProtocolVersion = 1

// Connection tuning.
const (
	// sendBuffer is how many messages may queue for one client before the
	// slow-client policy applies. A token-by-token model stream is bursty, so
	// the buffer is generous.
	sendBuffer = 512
	// maxDropped is how many messages one client may lose before it is
	// disconnected. Dropping is survivable — the frontend can refetch state —
	// but a client that never keeps up is just holding memory.
	maxDropped = 1024
	// writeTimeout bounds a single frame write to a stalled peer.
	writeTimeout = 10 * time.Second
	// pingInterval and pongTimeout detect a peer that has gone away without
	// closing, which is the normal outcome of a laptop lid closing.
	pingInterval = 30 * time.Second
	pongTimeout  = 10 * time.Second
	// readLimit bounds a single client message.
	readLimit = 1 << 20
	// closeGrace bounds the closing handshake per connection during shutdown.
	// A peer that is reading answers in microseconds; one that is not should
	// not be able to hold the process open while the user waits at a prompt.
	closeGrace = 2 * time.Second
)

// Server-to-client message types.
const (
	// MsgHello is sent once on connect with the current state snapshot.
	MsgHello = "hello"
	// MsgEvent wraps an app.Event forwarded from the core bus, unchanged.
	MsgEvent = "event"
	// MsgApproval reports a change to the approval queue.
	MsgApproval = "approval"
	// MsgAck confirms a client request, echoing its id.
	MsgAck = "ack"
	// MsgError reports a rejected client request.
	MsgError = "error"
	// MsgPong answers a client ping.
	MsgPong = "pong"
	// MsgDropped reports that the client fell behind and lost messages.
	MsgDropped = "dropped"
)

// Client-to-server message types.
const (
	// ClientPing requests a pong; the transport also pings on its own.
	ClientPing = "ping"
	// ClientSubscribe narrows the event types this connection receives.
	ClientSubscribe = "subscribe"
	// ClientApproval answers a pending approval (§50).
	ClientApproval = "approval"
	// ClientMessage submits a user turn; it runs asynchronously and its
	// progress arrives as events.
	ClientMessage = "message"
	// ClientCancel interrupts the running turn for a session (§51).
	ClientCancel = "cancel"
)

// ServerMessage is the envelope of everything the server sends.
//
// Exactly one of Event, Data and Error is set, chosen by Type.
type ServerMessage struct {
	Type string `json:"type"`
	// ID echoes the id of the client message this answers, when it answers one.
	ID string `json:"id,omitempty"`
	// Event carries a core bus event verbatim. The WebUI and the TUI consume
	// the same vocabulary (§25); this transport adds no events of its own.
	Event *app.Event `json:"event,omitempty"`
	Data  any        `json:"data,omitempty"`
	Error *errorBody `json:"error,omitempty"`
}

// ClientMessageEnvelope is the envelope of everything a client sends.
type ClientMessageEnvelope struct {
	Type string `json:"type"`
	// ID is an opaque client-chosen correlation id, echoed on the ack.
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// helloData is the connect-time snapshot.
type helloData struct {
	Protocol         int                           `json:"protocol"`
	ServerTime       time.Time                     `json:"server_time"`
	SessionID        string                        `json:"session_id,omitempty"`
	Mode             string                        `json:"mode"`
	PendingApprovals []permissions.PendingApproval `json:"pending_approvals"`
	Grants           []permissions.Grant           `json:"grants"`
}

// subscribeData narrows the event filter. An empty list means every event.
type subscribeData struct {
	Types []app.EventType `json:"types"`
}

// cancelData names the session whose turn should be interrupted.
type cancelData struct {
	SessionID string `json:"session_id,omitempty"`
}

// droppedData reports lost messages to a client that fell behind.
type droppedData struct {
	Count int64 `json:"count"`
}

// ---------------------------------------------------------------------------
// Hub
// ---------------------------------------------------------------------------

// hub tracks connected clients and fans messages out to them.
//
// Every send is non-blocking. The bus publishes on the goroutine that produced
// the event — often the model loop — so a slow browser must never be able to
// stall the runtime (§25).
type hub struct {
	mu      sync.Mutex
	closed  bool
	clients map[*wsClient]struct{}
}

func newHub() *hub { return &hub{clients: make(map[*wsClient]struct{})} }

// add registers a client, reporting false once the hub is shutting down.
func (h *hub) add(c *wsClient) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.clients[c] = struct{}{}
	return true
}

// remove deregisters a client.
func (h *hub) remove(c *wsClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// count returns the number of connected clients.
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// snapshot copies the client set so fan-out happens without holding the lock.
func (h *hub) snapshot() []*wsClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	return out
}

// broadcastEvent forwards a core event to every interested client.
//
// The payload is marshalled once for all clients: the same bytes go to each
// connection, and per-client work is reduced to a channel send.
func (h *hub) broadcastEvent(ev app.Event) {
	if h.count() == 0 {
		return
	}
	payload, err := json.Marshal(ServerMessage{Type: MsgEvent, Event: &ev})
	if err != nil {
		// A payload that will not marshal is a core bug, but dropping the one
		// event is better than tearing down every connection over it.
		return
	}
	for _, c := range h.snapshot() {
		if c.wants(ev.Type) {
			c.enqueue(payload)
		}
	}
}

// broadcastApproval forwards an approval queue change to every client.
// Approvals bypass the event filter: they are never optional (§50).
func (h *hub) broadcastApproval(ev permissions.ApprovalEvent) {
	if h.count() == 0 {
		return
	}
	payload, err := json.Marshal(ServerMessage{Type: MsgApproval, Data: ev})
	if err != nil {
		return
	}
	for _, c := range h.snapshot() {
		c.enqueue(payload)
	}
}

// shutdown closes every connection and waits for its goroutines to finish, so
// a browser sees a close frame rather than a severed TCP connection (§58).
func (h *hub) shutdown(ctx context.Context) {
	h.mu.Lock()
	h.closed = true
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		c.stop(websocket.StatusGoingAway, "boop is shutting down")
	}
	waitCtx, cancel := context.WithTimeout(ctx, closeGrace)
	defer cancel()
	for _, c := range clients {
		select {
		case <-c.finished:
		case <-waitCtx.Done():
			// The grace period expired. The close frame has already been
			// written; what is outstanding is the peer's reply, and the
			// library serialises CloseNow behind that same handshake — so
			// dropping the connection happens off this goroutine. Shutdown
			// must not wait on a client that has stopped reading.
			go c.hardClose()
		}
	}
}

// ---------------------------------------------------------------------------
// Client connection
// ---------------------------------------------------------------------------

// eventFilter is the set of event types a connection asked for; nil means all.
type eventFilter map[app.EventType]struct{}

// wsClient is one connected WebSocket peer.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
	// kill is closed to ask the write pump to close the connection.
	kill     chan struct{}
	killOnce sync.Once
	// finished is closed when both pumps have stopped.
	finished chan struct{}

	dropped atomic.Int64
	filter  atomic.Pointer[eventFilter]

	closeCode   atomic.Int64
	closeReason atomic.Pointer[string]
}

func newClient(conn *websocket.Conn) *wsClient {
	return &wsClient{
		conn:     conn,
		send:     make(chan []byte, sendBuffer),
		kill:     make(chan struct{}),
		finished: make(chan struct{}),
	}
}

// wants reports whether this connection subscribed to an event type.
func (c *wsClient) wants(t app.EventType) bool {
	f := c.filter.Load()
	if f == nil || *f == nil {
		return true
	}
	_, ok := (*f)[t]
	return ok
}

// setFilter replaces the subscription. An empty list restores "everything".
func (c *wsClient) setFilter(types []app.EventType) {
	if len(types) == 0 {
		c.filter.Store(nil)
		return
	}
	f := make(eventFilter, len(types))
	for _, t := range types {
		f[t] = struct{}{}
	}
	c.filter.Store(&f)
}

// enqueue queues a message without ever blocking.
//
// This is the slow-client policy (§25): a full buffer drops the message and
// counts it, and a client that keeps dropping is disconnected. The alternative
// — blocking — would let one stalled browser tab freeze the agent runtime.
func (c *wsClient) enqueue(payload []byte) {
	select {
	case c.send <- payload:
	default:
		if c.dropped.Add(1) >= maxDropped {
			c.stop(websocket.StatusPolicyViolation, "client could not keep up with the event stream")
		}
	}
}

// stop asks the write pump to close the connection with a status.
func (c *wsClient) stop(code websocket.StatusCode, reason string) {
	c.killOnce.Do(func() {
		c.closeCode.Store(int64(code))
		c.closeReason.Store(&reason)
		close(c.kill)
	})
}

// hardClose drops the connection without a handshake, for a peer that will
// not accept a clean close.
func (c *wsClient) hardClose() { _ = c.conn.CloseNow() }

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleEvents upgrades a connection and bridges it to the core event bus.
//
// Authentication and origin validation happen before the upgrade, and both are
// stricter than on the REST API. A browser always labels a WebSocket handshake
// with an Origin, so a missing one means a client that is not a browser
// pretending to be one — and unlike a cross-origin fetch, a hostile page's
// WebSocket is not blocked by the browser itself: the server is the only thing
// standing between a malicious tab and a loopback socket that can run
// commands (§23).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if err := s.auth.authenticate(r); err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="boop"`)
		writeError(w, http.StatusUnauthorized, codeUnauthorized,
			"a valid access token is required; send it as `Authorization: Bearer <token>`, "+
				"as the `"+tokenSubprotocolPrefix+"<token>` WebSocket subprotocol, or as ?"+tokenQueryParam+"=<token>")
		return
	}
	origin, present, allowed := s.origins.allowOrigin(r)
	if !present {
		writeError(w, http.StatusForbidden, codeForbidden,
			"the WebSocket handshake must carry an Origin header matching this server or web.allowed_origins")
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, codeForbidden,
			"origin "+origin+" is not allowed; add it to web.allowed_origins")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: negotiateSubprotocol(r),
		// Origin verification is done above, against the configured allowlist
		// and the request's own origin. The library's OriginPatterns cannot
		// express "scheme included, no globbing", so its check is turned off
		// rather than layered on top of a weaker one.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.log.Printf("web: websocket upgrade from %s failed: %v", clientIP(r, s.web.TrustedProxyHeaders), err)
		return
	}
	conn.SetReadLimit(readLimit)

	client := newClient(conn)
	if !s.hub.add(client) {
		_ = conn.Close(websocket.StatusGoingAway, "boop is shutting down")
		return
	}

	ctx, cancel := context.WithCancel(s.baseCtx)
	defer cancel()

	var pumps sync.WaitGroup
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		s.writePump(ctx, client)
	}()

	s.sendHello(client)
	s.readPump(ctx, client)

	client.stop(websocket.StatusNormalClosure, "")
	cancel()
	pumps.Wait()
	s.hub.remove(client)
	close(client.finished)
}

// sendHello delivers the connect-time snapshot, so a client that reconnects
// mid-run is immediately consistent instead of waiting for the next event.
func (s *Server) sendHello(c *wsClient) {
	data := helloData{
		Protocol:         ProtocolVersion,
		ServerTime:       s.now(),
		SessionID:        s.CurrentSession(),
		Mode:             string(s.cfg.Execution.Mode),
		PendingApprovals: []permissions.PendingApproval{},
		Grants:           []permissions.Grant{},
	}
	if s.broker != nil {
		if pending := s.broker.Pending(); pending != nil {
			data.PendingApprovals = pending
		}
		if grants := s.broker.SessionGrants(); grants != nil {
			data.Grants = grants
		}
	}
	c.enqueueMessage(ServerMessage{Type: MsgHello, Data: data})
}

// enqueueMessage marshals and queues one message.
func (c *wsClient) enqueueMessage(msg ServerMessage) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	c.enqueue(payload)
}

// writePump owns every write to the connection: a WebSocket allows only one
// concurrent writer, and funnelling pings, data and the close frame through
// one goroutine is what guarantees that.
func (s *Server) writePump(ctx context.Context, c *wsClient) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	var reportedDrops int64
	for {
		select {
		case <-ctx.Done():
			s.closeConn(c, websocket.StatusGoingAway, "boop is shutting down")
			return
		case <-c.kill:
			code := websocket.StatusCode(c.closeCode.Load())
			reason := ""
			if r := c.closeReason.Load(); r != nil {
				reason = *r
			}
			s.closeConn(c, code, reason)
			return
		case payload := <-c.send:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(writeCtx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				c.hardClose()
				return
			}
		case <-ticker.C:
			// Tell a client that fell behind that it did, before pinging: a
			// gap in the event stream it does not know about is worse than a
			// gap it can react to by refetching.
			if dropped := c.dropped.Load(); dropped > reportedDrops {
				reportedDrops = dropped
				notice, err := json.Marshal(ServerMessage{Type: MsgDropped, Data: droppedData{Count: dropped}})
				if err == nil {
					writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
					_ = c.conn.Write(writeCtx, websocket.MessageText, notice)
					cancel()
				}
			}
			pingCtx, cancel := context.WithTimeout(ctx, pongTimeout)
			err := c.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				c.hardClose()
				return
			}
		}
	}
}

// closeConn performs the closing handshake, falling back to a hard close.
func (s *Server) closeConn(c *wsClient, code websocket.StatusCode, reason string) {
	if code == 0 {
		code = websocket.StatusNormalClosure
	}
	if err := c.conn.Close(code, reason); err != nil {
		c.hardClose()
	}
}

// readPump handles client requests until the connection ends.
func (s *Server) readPump(ctx context.Context, c *wsClient) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			c.enqueueMessage(errorMessage("", codeBadRequest, "only text frames are accepted"))
			continue
		}
		var msg ClientMessageEnvelope
		if err := json.Unmarshal(data, &msg); err != nil {
			c.enqueueMessage(errorMessage("", codeBadRequest, "the message is not valid JSON: "+err.Error()))
			continue
		}
		s.dispatchClientMessage(c, msg)
	}
}

// dispatchClientMessage routes one client request.
//
// Every branch delegates to the same code the REST API uses, so the two
// transports cannot drift apart.
func (s *Server) dispatchClientMessage(c *wsClient, msg ClientMessageEnvelope) {
	switch msg.Type {
	case ClientPing:
		c.enqueueMessage(ServerMessage{Type: MsgPong, ID: msg.ID})

	case ClientSubscribe:
		var data subscribeData
		if !decodeClientData(c, msg, &data) {
			return
		}
		c.setFilter(data.Types)
		c.enqueueMessage(ServerMessage{Type: MsgAck, ID: msg.ID, Data: data})

	case ClientApproval:
		var req approvalRequest
		if !decodeClientData(c, msg, &req) {
			return
		}
		result, status, err := s.applyApproval(req)
		if err != nil {
			c.enqueueMessage(errorMessage(msg.ID, codeForApprovalError(status), err.Error()))
			return
		}
		c.enqueueMessage(ServerMessage{Type: MsgAck, ID: msg.ID, Data: result})

	case ClientMessage:
		var req messageRequest
		if !decodeClientData(c, msg, &req) {
			return
		}
		if s.app == nil {
			c.enqueueMessage(errorMessage(msg.ID, codeUnavailable, "this server was started without a Boop runtime attached"))
			return
		}
		if req.Content == "" {
			c.enqueueMessage(errorMessage(msg.ID, codeBadRequest, "`content` must not be empty"))
			return
		}
		// Attachments are prepared before the turn starts so an unreadable
		// file or an incapable model is reported on this socket rather than
		// surfacing later as a bare error event (§27, §8).
		attachments, err := s.prepareTurn(s.baseCtx, req, nil)
		if err != nil {
			var ae *attachmentError
			code := codeBadRequest
			if errors.As(err, &ae) {
				code = ae.code
			}
			c.enqueueMessage(errorMessage(msg.ID, code, err.Error()))
			return
		}
		sessionID, err := s.resolveSession(s.baseCtx, req.SessionID)
		if err != nil {
			c.enqueueMessage(errorMessage(msg.ID, codeInternal, err.Error()))
			return
		}
		// Always asynchronous over the socket: the answer is the event
		// stream, which is the whole reason this transport exists.
		if _, err := s.startTurn(s.baseCtx, sessionID, req, attachments); err != nil {
			c.enqueueMessage(errorMessage(msg.ID, codeForTurnError(err), err.Error()))
			return
		}
		c.enqueueMessage(ServerMessage{Type: MsgAck, ID: msg.ID, Data: map[string]any{"session_id": sessionID, "accepted": true}})

	case ClientCancel:
		var data cancelData
		if !decodeClientData(c, msg, &data) {
			return
		}
		sessionID := data.SessionID
		if sessionID == "" {
			sessionID = s.CurrentSession()
		}
		cancelled := s.cancelTurn(sessionID)
		c.enqueueMessage(ServerMessage{Type: MsgAck, ID: msg.ID,
			Data: map[string]any{"session_id": sessionID, "cancelled": cancelled}})

	default:
		c.enqueueMessage(errorMessage(msg.ID, codeBadRequest, "unknown message type "+msg.Type))
	}
}

// decodeClientData unmarshals the data payload of a client message, answering
// the client on failure.
func decodeClientData(c *wsClient, msg ClientMessageEnvelope, dst any) bool {
	if len(msg.Data) == 0 {
		return true
	}
	if err := json.Unmarshal(msg.Data, dst); err != nil {
		c.enqueueMessage(errorMessage(msg.ID, codeBadRequest, "the `data` field is malformed: "+err.Error()))
		return false
	}
	return true
}

// errorMessage builds an error envelope matching the REST API's shape.
func errorMessage(id, code, message string) ServerMessage {
	return ServerMessage{Type: MsgError, ID: id, Error: &errorBody{Code: code, Message: message}}
}
