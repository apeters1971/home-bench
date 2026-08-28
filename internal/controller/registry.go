package controller

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apeters/homebench/internal/protocol"
)

// Registry tracks connected clients and assigns prefixes by hostname hash.
type Registry struct {
	mu      sync.RWMutex
	clients map[string]*regEntry
}

type regEntry struct {
	info     protocol.ClientInfo
	lastSeen atomic.Int64 // unix nano; updated without the registry write lock
}

func NewRegistry() *Registry {
	return &Registry{clients: make(map[string]*regEntry)}
}

func (r *Registry) Upsert(id, hostname, prefix string) protocol.ClientInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	info := protocol.ClientInfo{
		ID:       id,
		Hostname: hostname,
		Prefix:   prefix,
		LastSeen: now,
		Status:   "registered",
		Phase:    protocol.PhaseIdle,
	}
	e := &regEntry{info: info}
	e.lastSeen.Store(now.UnixNano())
	r.clients[id] = e
	return info
}

func (r *Registry) Touch(id string) {
	r.mu.RLock()
	e := r.clients[id]
	r.mu.RUnlock()
	if e != nil {
		e.lastSeen.Store(time.Now().UnixNano())
	}
}

func (r *Registry) SetStatus(id, status string, phase protocol.Phase) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.clients[id]; ok {
		e.info.Status = status
		e.info.Phase = phase
		now := time.Now()
		e.info.LastSeen = now
		e.lastSeen.Store(now.UnixNano())
	}
}

func (r *Registry) Lookup(id string) (protocol.ClientInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.clients[id]
	if !ok || e == nil {
		return protocol.ClientInfo{}, false
	}
	cp := e.info
	cp.LastSeen = time.Unix(0, e.lastSeen.Load())
	return cp, true
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
	for _, e := range r.clients {
		cp := e.info
		cp.LastSeen = time.Unix(0, e.lastSeen.Load())
		out = append(out, cp)
	}
	return out
}

// ListCapped returns up to limit clients sorted by hostname, then id.
func (r *Registry) ListCapped(limit int) []protocol.ClientInfo {
	all := r.List()
	sort.Slice(all, func(i, j int) bool {
		if all[i].Hostname != all[j].Hostname {
			return all[i].Hostname < all[j].Hostname
		}
		return all[i].ID < all[j].ID
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

// Statuses returns status strings for the given IDs (missing IDs omitted).
func (r *Registry) Statuses(ids []string) map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if e, ok := r.clients[id]; ok {
			out[id] = e.info.Status
		}
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

// IDsSorted returns all client IDs ordered by hostname, then id.
func (r *Registry) IDsSorted() []string {
	all := r.List()
	sort.Slice(all, func(i, j int) bool {
		if all[i].Hostname != all[j].Hostname {
			return all[i].Hostname < all[j].Hostname
		}
		return all[i].ID < all[j].ID
	})
	out := make([]string, len(all))
	for i := range all {
		out[i] = all[i].ID
	}
	return out
}

func (r *Registry) ReassignPrefixes(prefixes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.clients {
		e.info.Prefix = protocol.SelectPrefix(e.info.Hostname, prefixes)
	}
}

// Prune removes clients that have not been seen within ttl.
func (r *Registry) Prune(ttl time.Duration) []protocol.ClientInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-ttl).UnixNano()
	var gone []protocol.ClientInfo
	for id, e := range r.clients {
		if e.lastSeen.Load() < cutoff {
			cp := e.info
			cp.LastSeen = time.Unix(0, e.lastSeen.Load())
			gone = append(gone, cp)
			delete(r.clients, id)
		}
	}
	return gone
}
