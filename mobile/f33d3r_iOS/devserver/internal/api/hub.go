package api

import (
	"encoding/json"
	"sync"
)

// hub is the in-process fan-out behind the SSE routes: one topic per live
// room, one per user. It is feed-engine's sse_registry with the account and
// PIAL pipes collapsed into named topics, and — like the real one — it is
// deliberately not persisted: presence ends with the connection.
type hub struct {
	mu     sync.Mutex
	topics map[string]map[*subscriber]struct{}
}

type subscriber struct {
	ch     chan frame
	userID string
}

// frame is one SSE event: a name and a JSON payload. The web's stream carries
// rendered HTML; a native client has no DOM to swap it into, so this stream
// carries the plan's "signal frames" — small JSON the app renders itself.
type frame struct {
	Event string
	Data  []byte
}

func newHub() *hub { return &hub{topics: map[string]map[*subscriber]struct{}{}} }

// subscribe returns a channel of frames for topic and a cancel func.
func (h *hub) subscribe(topic, userID string) (*subscriber, func()) {
	sub := &subscriber{ch: make(chan frame, 64), userID: userID}
	h.mu.Lock()
	if h.topics[topic] == nil {
		h.topics[topic] = map[*subscriber]struct{}{}
	}
	h.topics[topic][sub] = struct{}{}
	h.mu.Unlock()
	return sub, func() {
		h.mu.Lock()
		if subs := h.topics[topic]; subs != nil {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(h.topics, topic)
			}
		}
		h.mu.Unlock()
	}
}

// publish sends an event to every subscriber of topic. A slow subscriber
// drops the frame rather than blocking the publisher.
func (h *hub) publish(topic, event string, payload any) {
	data, _ := json.Marshal(payload)
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.topics[topic] {
		select {
		case sub.ch <- frame{Event: event, Data: data}:
		default:
		}
	}
}

// publishTo sends an event to the subscribers on topic who belong to one of
// the named accounts. It is how a frame that is not everybody's business — the
// microphone queue, which only moderators may read — reaches the people it is
// for without opening a second topic that a role change would leave stale.
func (h *hub) publishTo(topic, event string, payload any, userIDs []string) {
	if len(userIDs) == 0 {
		return
	}
	allowed := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		allowed[id] = true
	}
	data, _ := json.Marshal(payload)
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.topics[topic] {
		if !allowed[sub.userID] {
			continue
		}
		select {
		case sub.ch <- frame{Event: event, Data: data}:
		default:
		}
	}
}

// presence counts distinct users subscribed to topic, excluding `except`.
func (h *hub) presence(topic, except string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	for sub := range h.topics[topic] {
		if sub.userID == "" || sub.userID == except {
			continue
		}
		seen[sub.userID] = true
	}
	return len(seen)
}

// audience lists the distinct user ids on a topic.
func (h *hub) audience(topic string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for sub := range h.topics[topic] {
		if sub.userID != "" && !seen[sub.userID] {
			seen[sub.userID] = true
			out = append(out, sub.userID)
		}
	}
	return out
}
