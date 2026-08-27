package controller

import (
	"sync"
	"time"

	"github.com/apeters/homebench/internal/protocol"
)

// Registry tracks connected clients and assigns prefixes by hostname hash.
type Registry struct {
	mu      sync.RWMutex
	clients map[string]*protocol.ClientInfo
}

func NewRegistry() *Registry {
	return &Registry{clients: make(map[string]*protocol.ClientInfo)}
}

func (r *Registry) Upsert(id, hostname, prefix string) protocol.ClientInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	info := &protocol.ClientInfo{
		ID:       id,
		Hostname: hostname,
		Prefix:   prefix,
		LastSeen: time.Now(),
		Status:   "registered",
		Phase:    protocol.PhaseIdle,
	}
	r.clients[id] = info
	return *info
}

func (r *Registry) Touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[id]; ok {
		c.LastSeen = time.Now()
	}
}

func (r *Registry) SetStatus(id, status string, phase protocol.Phase) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[id]; ok {
		c.Status = status
		c.Phase = phase
		c.LastSeen = time.Now()
	}
}

func (r *Registry) Lookup(id string) (protocol.ClientInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.clients[id]
	if !ok || c == nil {
		return protocol.ClientInfo{}, false
	}
	return *c, true
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, id)
}

func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

func (r *Registry) List() []protocol.ClientInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]protocol.ClientInfo, 0, len(r.clients))
	for _, c := range r.clients {
		cp := *c
		out = append(out, cp)
	}
	return out
}

func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.clients))
	for id := range r.clients {
		out = append(out, id)
	}
	return out
}

func (r *Registry) ReassignPrefixes(prefixes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.clients {
		c.Prefix = protocol.SelectPrefix(c.Hostname, prefixes)
	}
}

// Prune removes clients that have not been seen within ttl.
func (r *Registry) Prune(ttl time.Duration) []protocol.ClientInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-ttl)
	var gone []protocol.ClientInfo
	for id, c := range r.clients {
		if c.LastSeen.Before(cutoff) {
			cp := *c
			gone = append(gone, cp)
			delete(r.clients, id)
		}
	}
	return gone
}
