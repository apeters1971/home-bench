package controller

import (
	"fmt"
	"strings"
)

// ParticipantUpdate changes who will run the next test. Modes operate on the
// full registry so the UI can select thousands while only showing a capped list.
type ParticipantUpdate struct {
	Mode      string   `json:"mode,omitempty"` // "all", "none", "first_n", or empty for set/add/remove
	N         int      `json:"n,omitempty"`
	ClientIDs []string `json:"client_ids,omitempty"` // replace selection when mode empty and Add/Remove empty
	Add       []string `json:"add,omitempty"`
	Remove    []string `json:"remove,omitempty"`
}

// UpdateParticipants applies a selection change while idle.
func (o *Orchestrator) UpdateParticipants(u ParticipantUpdate) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.running {
		return fmt.Errorf("cannot change participants while running")
	}

	known := make(map[string]struct{})
	for _, id := range o.registry.IDs() {
		known[id] = struct{}{}
	}

	mode := strings.ToLower(strings.TrimSpace(u.Mode))
	switch mode {
	case "all":
		next := make(map[string]struct{}, len(known))
		for id := range known {
			next[id] = struct{}{}
		}
		o.selectedIDs = next
		o.autoSelectNew = len(next) > 0
		return nil
	case "none":
		o.selectedIDs = make(map[string]struct{})
		o.autoSelectNew = false
		return nil
	case "first_n":
		ids := o.registry.IDsSorted()
		n := u.N
		if n < 0 {
			n = 0
		}
		if n > len(ids) {
			n = len(ids)
		}
		next := make(map[string]struct{}, n)
		for _, id := range ids[:n] {
			next[id] = struct{}{}
		}
		o.selectedIDs = next
		o.autoSelectNew = false
		return nil
	}

	if len(u.Add) > 0 || len(u.Remove) > 0 {
		if o.selectedIDs == nil {
			o.selectedIDs = make(map[string]struct{})
		}
		for _, id := range u.Add {
			id = strings.TrimSpace(id)
			if _, ok := known[id]; ok {
				o.selectedIDs[id] = struct{}{}
			}
		}
		for _, id := range u.Remove {
			delete(o.selectedIDs, strings.TrimSpace(id))
		}
		o.autoSelectNew = len(o.selectedIDs) > 0 && len(o.selectedIDs) == len(known)
		return nil
	}

	// Legacy replace-with-list (small fleets / tests).
	next := make(map[string]struct{}, len(u.ClientIDs))
	for _, id := range u.ClientIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := known[id]; !ok {
			continue
		}
		next[id] = struct{}{}
	}
	o.selectedIDs = next
	o.autoSelectNew = len(next) > 0 && len(next) == len(known)
	return nil
}
