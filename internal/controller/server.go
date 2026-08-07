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

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Server is the central controller HTTP + WebSocket endpoint.
type Server struct {
	registry *Registry
	orch     *Orchestrator
	metrics  *MetricsStore
	webFS    fs.FS

	mu      sync.RWMutex
	clients map[string]*wsClient // clientID -> conn
	uiSubs  map[*websocket.Conn]struct{}

	uiMu       sync.Mutex
	uiPending  bool
	lastUIPush time.Time
}

type wsClient struct {
	id   string
	conn *websocket.Conn
	mu   sync.Mutex
}

func NewServer(webFS fs.FS) *Server {
	reg := NewRegistry()
	metrics := NewMetricsStore()
	s := &Server{
		registry: reg,
		metrics:  metrics,
		webFS:    webFS,
		clients:  make(map[string]*wsClient),
		uiSubs:   make(map[*websocket.Conn]struct{}),
	}
	s.orch = NewOrchestrator(reg, metrics, s)
	return s
}

func (s *Server) Broadcast(env protocol.Envelope) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.clients {
		c.send(env)
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

func (c *wsClient) send(env protocol.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.conn.WriteJSON(env); err != nil {
		log.Printf("send to %s: %v", c.id, err)
	}
}

func (c *wsClient) ping() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteMessage(websocket.PingMessage, []byte("ping"))
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/config", s.handleConfig)
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
			for _, id := range s.registry.Prune(protocol.ClientStaleAfter) {
				s.mu.Lock()
				if c, ok := s.clients[id]; ok {
					_ = c.conn.Close()
					delete(s.clients, id)
				}
				s.mu.Unlock()
				log.Printf("pruned stale client %s", id)
			}
			s.flushUI()
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

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.orch.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.flushUI()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.orch.Stop()
	s.flushUI()
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
			if cur, ok := s.clients[clientID]; ok && cur.conn == conn {
				delete(s.clients, clientID)
				s.registry.Remove(clientID)
				log.Printf("client disconnected: %s", clientID)
			}
			s.mu.Unlock()
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
		// Reply with pong using the client's write lock once registered.
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
			wc = &wsClient{id: id, conn: conn}
			s.mu.Lock()
			// Close any superseded connection for this ID.
			if old, ok := s.clients[id]; ok && old.conn != conn {
				_ = old.conn.Close()
			}
			s.clients[id] = wc
			s.mu.Unlock()
			welcome := protocol.WelcomeMsg{ClientID: id, Prefix: prefix, Config: cfg}
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
			s.registry.Touch(env.Metrics.ClientID)
			s.metrics.Add(*env.Metrics)
			s.requestUI()

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
	s.mu.Lock()
	s.uiSubs[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.uiSubs, conn)
		s.mu.Unlock()
		_ = conn.Close()
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
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	_ = conn.WriteJSON(s.orch.Snapshot())
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
	if time.Since(s.lastUIPush) >= 500*time.Millisecond {
		s.lastUIPush = time.Now()
		s.uiPending = false
		go s.flushUI()
		return
	}
	s.uiPending = true
}

func (s *Server) flushUI() {
	s.uiMu.Lock()
	s.uiPending = false
	s.lastUIPush = time.Now()
	s.uiMu.Unlock()

	snap := s.orch.Snapshot()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for conn := range s.uiSubs {
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := conn.WriteJSON(snap); err != nil {
			_ = conn.Close()
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
