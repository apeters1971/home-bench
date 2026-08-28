package controller

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/apeters/homebench/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const clientSendQueue = 64

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Server is the central controller HTTP + WebSocket endpoint.
type Server struct {
	registry *Registry
	orch     *Orchestrator
	metrics  *MetricsStore
	webFS    fs.FS

	mu      sync.RWMutex
	clients map[string]*wsClient // clientID -> conn
	uiSubs  map[*uiConn]struct{}

	uiMu       sync.Mutex
	uiPending  bool
	uiFlushing bool
	lastUIPush time.Time
}

type wsClient struct {
	id   string
	conn *websocket.Conn
	out  chan []byte
	done chan struct{}
	mu   sync.Mutex // serializes WriteMessage (queue + ping/pong)
}

// uiConn serializes all writes to a browser WebSocket (snapshots + pings).
type uiConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (u *uiConn) writeJSON(v any) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	_ = u.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return u.conn.WriteJSON(v)
}

func (u *uiConn) ping() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	_ = u.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return u.conn.WriteMessage(websocket.PingMessage, []byte("ping"))
}

func (u *uiConn) close() {
	_ = u.conn.Close()
}

func NewServer(webFS fs.FS, configPath string) *Server {
	reg := NewRegistry()
	metrics := NewMetricsStore()
	s := &Server{
		registry: reg,
		metrics:  metrics,
		webFS:    webFS,
		clients:  make(map[string]*wsClient),
		uiSubs:   make(map[*uiConn]struct{}),
	}
	s.orch = NewOrchestrator(reg, metrics, s, configPath)
	return s
}

func newWSClient(id string, conn *websocket.Conn) *wsClient {
	c := &wsClient{
		id:   id,
		conn: conn,
		out:  make(chan []byte, clientSendQueue),
		done: make(chan struct{}),
	}
	go c.writeLoop()
	return c
}

func (c *wsClient) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case raw := <-c.out:
			c.mu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := c.conn.WriteMessage(websocket.TextMessage, raw)
			c.mu.Unlock()
			if err != nil {
				_ = c.conn.Close()
				return
			}
		}
	}
}

func (c *wsClient) stop() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

func (c *wsClient) enqueue(raw []byte) {
	select {
	case c.out <- raw:
		return
	default:
	}
	// Drop oldest queued message to make room for a fresher command.
	select {
	case <-c.out:
	default:
	}
	select {
	case c.out <- raw:
	default:
		log.Printf("send queue full for %s — dropping message", c.id)
	}
}

func (c *wsClient) send(env protocol.Envelope) {
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	c.enqueue(raw)
}

func (c *wsClient) ping() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteMessage(websocket.PingMessage, []byte("ping"))
}

func (s *Server) Broadcast(env protocol.Envelope) {
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	s.mu.RLock()
	targets := make([]*wsClient, 0, len(s.clients))
	for _, c := range s.clients {
		targets = append(targets, c)
	}
	s.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(raw)
	}
}

func (s *Server) BroadcastTo(ids []string, env protocol.Envelope) {
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	s.mu.RLock()
	targets := make([]*wsClient, 0, len(ids))
	for _, id := range ids {
		if c := s.clients[id]; c != nil {
			targets = append(targets, c)
		}
	}
	s.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(raw)
	}
}

func (s *Server) SendTo(clientID string, env protocol.Envelope) {
	s.mu.RLock()
	c := s.clients[clientID]
	s.mu.RUnlock()
	if c != nil {
		c.send(env)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/participants", s.handleParticipants)
	mux.HandleFunc("/api/start", s.handleStart)
	mux.HandleFunc("/api/stop", s.handleStop)
	mux.HandleFunc("/ws/client", s.handleClientWS)
	mux.HandleFunc("/ws/ui", s.handleUIWS)
	if s.webFS != nil {
		mux.Handle("/", http.FileServer(http.FS(s.webFS)))
	}
	return mux
}

func (s *Server) StartBackground() {
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			for _, info := range s.registry.Prune(protocol.ClientStaleAfter) {
				s.mu.Lock()
				if c, ok := s.clients[info.ID]; ok {
					c.stop()
					_ = c.conn.Close()
					delete(s.clients, info.ID)
				}
				s.mu.Unlock()
				s.orch.OnClientRemoved(info.ID, info.Hostname)
				log.Printf("pruned stale client %s", info.ID)
			}
			s.requestUI()
		}
	}()
	// Steady UI refresh while a run is active (metrics no longer poke the UI each sample).
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			if s.orch.IsRunning() {
				s.requestUI()
			}
		}
	}()
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.orch.Snapshot())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.orch.Config())
	case http.MethodPut, http.MethodPost:
		var cfg protocol.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.orch.SetConfig(cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, cfg)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleParticipants(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut, http.MethodPost:
		var body ParticipantUpdate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.orch.UpdateParticipants(body); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		s.requestUI()
		writeJSON(w, s.orch.Snapshot())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.orch.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.requestUI()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.orch.Stop()
	s.requestUI()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleClientWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("client ws upgrade: %v", err)
		return
	}

	var clientID string
	var wc *wsClient
	done := make(chan struct{})
	defer close(done)
	defer func() {
		// Only tear down registry state if this conn is still the active one.
		// A reconnect with the same ID must not be wiped by the old handler.
		if clientID != "" {
			s.mu.Lock()
			removed := false
			host := ""
			if cur, ok := s.clients[clientID]; ok && cur.conn == conn {
				if info, ok := s.registry.Lookup(clientID); ok {
					host = info.Hostname
				}
				cur.stop()
				delete(s.clients, clientID)
				s.registry.Remove(clientID)
				removed = true
				log.Printf("client disconnected: %s", clientID)
			}
			s.mu.Unlock()
			if removed {
				s.orch.OnClientRemoved(clientID, host)
			}
			s.requestUI()
		}
		_ = conn.Close()
	}()

	_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))
		if clientID != "" {
			s.registry.Touch(clientID)
		}
		return nil
	})
	conn.SetPingHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))
		if clientID != "" {
			s.registry.Touch(clientID)
		}
		if wc != nil {
			wc.mu.Lock()
			defer wc.mu.Unlock()
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			return conn.WriteMessage(websocket.PongMessage, []byte(appData))
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteMessage(websocket.PongMessage, []byte(appData))
	})

	go func() {
		t := time.NewTicker(protocol.WSPingInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if wc == nil {
					continue
				}
				if err := wc.ping(); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))

		switch env.Type {
		case "register":
			if env.Register == nil {
				continue
			}
			id := env.Register.ID
			if id == "" {
				id = uuid.NewString()
			}
			clientID = id
			cfg := s.orch.Config()
			prefix := protocol.SelectPrefix(env.Register.Hostname, cfg.Prefixes)
			info := s.registry.Upsert(id, env.Register.Hostname, prefix)
			s.orch.OnClientRegistered(id)
			wc = newWSClient(id, conn)
			s.mu.Lock()
			if old, ok := s.clients[id]; ok && old.conn != conn {
				old.stop()
				_ = old.conn.Close()
			}
			s.clients[id] = wc
			s.mu.Unlock()
			welcome := protocol.WelcomeMsg{
				ClientID:           id,
				Prefix:             prefix,
				Config:             cfg,
				MetricsIntervalSec: protocol.MetricsIntervalForClients(s.registry.Count()).Seconds(),
			}
			wc.send(protocol.Envelope{Type: "welcome", Welcome: &welcome})
			log.Printf("client registered: id=%s host=%s prefix=%s", id, info.Hostname, prefix)
			s.requestUI()

		case "metrics":
			if env.Metrics == nil {
				continue
			}
			if env.Metrics.Timestamp.IsZero() {
				env.Metrics.Timestamp = time.Now().UTC()
			}
			// Liveness comes from WS ping/pong; avoid a registry write lock per sample.
			if s.orch.IsParticipant(env.Metrics.ClientID) {
				s.metrics.Add(*env.Metrics)
			}

		case "heartbeat":
			if clientID != "" {
				s.registry.Touch(clientID)
			} else if env.Status != nil {
				s.registry.Touch(env.Status.ClientID)
			}

		case "status":
			if env.Status == nil {
				continue
			}
			s.registry.SetStatus(env.Status.ClientID, env.Status.Status, env.Status.Phase)
			if env.Status.Files > 0 {
				s.orch.UpdateClientFiles(env.Status.Files)
			}
			s.requestUI()
		}
	}
}

func (s *Server) handleUIWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ui := &uiConn{conn: conn}
	s.mu.Lock()
	s.uiSubs[ui] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.uiSubs, ui)
		s.mu.Unlock()
		ui.close()
	}()

	_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))
		return nil
	})

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(protocol.WSPingInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := ui.ping(); err != nil {
					ui.close()
					return
				}
			}
		}
	}()

	_ = ui.writeJSON(s.orch.Snapshot())
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))
	}
}

// requestUI marks the UI dirty; flushed at most ~2 Hz to avoid flooding browsers.
func (s *Server) requestUI() {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	s.uiPending = true
	if s.uiFlushing {
		return
	}
	if time.Since(s.lastUIPush) < 500*time.Millisecond {
		return
	}
	s.uiFlushing = true
	go s.flushUI()
}

func (s *Server) flushUI() {
	for {
		s.uiMu.Lock()
		if !s.uiPending {
			s.uiFlushing = false
			s.uiMu.Unlock()
			return
		}
		s.uiPending = false
		s.lastUIPush = time.Now()
		s.uiMu.Unlock()

		snap := s.orch.Snapshot()
		s.mu.RLock()
		subs := make([]*uiConn, 0, len(s.uiSubs))
		for ui := range s.uiSubs {
			subs = append(subs, ui)
		}
		s.mu.RUnlock()

		for _, ui := range subs {
			if err := ui.writeJSON(snap); err != nil {
				ui.close()
			}
		}

		s.uiMu.Lock()
		if s.uiPending {
			s.uiMu.Unlock()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		s.uiFlushing = false
		s.uiMu.Unlock()
		return
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
