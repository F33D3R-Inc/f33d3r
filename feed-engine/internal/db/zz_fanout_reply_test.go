package db

import (
	"strings"
	"testing"
)

// TestFanoutNeverAnnouncesReplies: a reply is not a feed item, so the fan-out
// audience query must carry the same is_reply gate every feed reads. Being a
// reply is the server's fact (works.is_reply), not the client's kind label, so a
// media reply or a thread continuation is excluded too.
func TestFanoutNeverAnnouncesReplies(t *testing.T) {
	if !strings.Contains(fanoutEligibleSQL, "NOT w.is_reply") {
		t.Fatal("fanoutEligibleSQL must exclude replies by works.is_reply")
	}
	if strings.Contains(fanoutEligibleSQL, "kind") {
		t.Fatal("fanoutEligibleSQL must not decide replies from the kind label")
	}
}
