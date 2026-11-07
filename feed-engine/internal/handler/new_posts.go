package handler

// new_posts.go — the "N new posts" pill on Following.
//
// A new post no longer travels as a finished card. One card rendered for nobody can't
// carry each reader's Follow state, menu or safety checks, so the stream only moves a
// number. When a person taps the pill, Following is read for them through the same
// pipeline as any other page. The count lives here on the server.

import (
	"bytes"
	"log"
	"sync"
)

const (
	newPostFacetID = "facet:f33d3r:feed:following:new_posts"
	// newPostCounterMax bounds the counter. Counts are a courtesy, so losing them when
	// it fills costs only a pill that restarts from zero.
	newPostCounterMax = 200000
)

var newPostCounts = struct {
	sync.Mutex
	byPerson map[string]int
}{byPerson: map[string]int{}}

func newPostCount(pial string) int {
	newPostCounts.Lock()
	defer newPostCounts.Unlock()
	return newPostCounts.byPerson[pial]
}

// renderNewPostBanner renders the pill for a count.
func (h *Handler) renderNewPostBanner(count int) string {
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "new_post_banner", map[string]interface{}{"NewPostCount": count}); err != nil {
		log.Printf("[new-posts] %v", err)
		return ""
	}
	return buf.String()
}

// tellNewPost raises the count for everyone who can see this work and shows it to them.
func (h *Handler) tellNewPost(pials []string) {
	rendered := map[int]string{}
	for _, pial := range pials {
		newPostCounts.Lock()
		if len(newPostCounts.byPerson) >= newPostCounterMax {
			newPostCounts.byPerson = map[string]int{}
		}
		newPostCounts.byPerson[pial]++
		count := newPostCounts.byPerson[pial]
		newPostCounts.Unlock()
		frag, ok := rendered[count]
		if !ok {
			frag = h.renderNewPostBanner(count)
			rendered[count] = frag
		}
		if frag != "" {
			PublishFacetToUser(pial, FacetMutation{FacetID: newPostFacetID, Fragment: frag, Priority: FacetPriorityLow})
		}
	}
}

// clearNewPosts is called when a person reads the top of Following.
func (h *Handler) clearNewPosts(pial string) {
	newPostCounts.Lock()
	had := newPostCounts.byPerson[pial] > 0
	delete(newPostCounts.byPerson, pial)
	newPostCounts.Unlock()
	if had {
		if frag := h.renderNewPostBanner(0); frag != "" {
			PublishFacetToUser(pial, FacetMutation{FacetID: newPostFacetID, Fragment: frag, Priority: FacetPriorityLow})
		}
	}
}
