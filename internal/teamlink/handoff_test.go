package teamlink

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/teamwire"
)

// The handoff pull and the member directory are signed over their route patterns like
// every device call; the pull names the handoff kind alone, and an older server's 400
// reaches the caller as a status it can park on.
func TestHandoffPullAndMembersAreSignedDeviceCalls(t *testing.T) {
	key, _ := NewKey()
	device := engine.NewTypedID(engine.DeviceIDPrefix)
	var origin string
	older := false
	var pulled teamwire.PullRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get(teamwire.HeaderTimestamp), 10, 64)
		sr := teamwire.SignedRequest{Origin: origin, DeviceID: r.Header.Get(teamwire.HeaderDevice), Method: r.Method, Route: r.URL.Path,
			Timestamp: ts, Nonce: r.Header.Get(teamwire.HeaderNonce), BodySHA256: r.Header.Get(teamwire.HeaderBodyDigest)}
		if teamwire.BodyDigest(body) != sr.BodySHA256 {
			t.Errorf("body digest mismatch on %s", r.URL.Path)
		}
		if err := teamwire.Verify(key.Public().(ed25519.PublicKey), sr, r.Header.Get(teamwire.HeaderSignature)); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case teamwire.RoutePull:
			_ = json.Unmarshal(body, &pulled)
			if older {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(teamwire.ErrorBody{Error: teamwire.Error{Code: "invalid_request", Message: "kinds"}})
				return
			}
			_ = json.NewEncoder(w).Encode(teamwire.PullResponse{Cursor: 4, Rows: []teamwire.PullRow{{Seq: 4, Kind: teamwire.KindHandoff,
				Handoff: &teamwire.PulledHandoff{ID: "hnd_x", ToMe: true, State: teamwire.HandoffSent}}}})
		case teamwire.RouteMembers:
			_ = json.NewEncoder(w).Encode(teamwire.MembersResponse{Self: "usr_me", Members: []teamwire.Member{{UserID: "usr_me", DisplayName: "Me"}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	origin, _ = teamwire.CanonicalOrigin(ts.URL)
	c, err := NewClient(ts.URL, device, key, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.PullHandoffs(3, true)
	if err != nil || page.Cursor != 4 || len(page.Rows) != 1 || page.Rows[0].Handoff == nil || page.Rows[0].Handoff.ID != "hnd_x" {
		t.Fatalf("pull: %+v err=%v", page, err)
	}
	if pulled.Cursor != 3 || len(pulled.Kinds) != 1 || pulled.Kinds[0] != teamwire.KindHandoff || !pulled.Bootstrap {
		t.Fatalf("the handoff pull is its own request, naming the handoff kind alone: %+v", pulled)
	}
	members, err := c.Members()
	if err != nil || members.Self != "usr_me" || len(members.Members) != 1 {
		t.Fatalf("members: %+v err=%v", members, err)
	}
	older = true
	_, err = c.PullHandoffs(0, false)
	var refused *ErrServer
	if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest {
		t.Fatalf("an older server's 400 is a server refusal with its status: %v", err)
	}
}
