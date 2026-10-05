package teamlink

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"crossing-guard/teamwire"
)

// The signed device calls of handoff between members (team rest-of-release plan §6.6):
// the handoff pull — the pull route asked for kinds ["handoff"], a request of its own
// with its own cursor, never mixed with the memory pull — and the member directory.
// Both are device-initiated, like every call here.

// handoffPullReadLimit bounds one pulled page as read off the wire. A page carries whole
// documents (the server caps a body at 200,000 bytes and an excerpt at 65,536), so the
// 1 MiB bound of the other calls is too small; a page past this is a server fault.
const handoffPullReadLimit = 32 << 20

// PullHandoffs asks for the handoff delivery rows after cursor. The server returns the
// document only to devices of the recipient; for handoffs this device's user sent it
// returns who, when and the state. An older server answers the request 400 (it knows
// only the memory kinds): the caller parks the handoff pull and leaves the memory pull,
// a different request, untouched.
//
// bootstrap is true on every page until this link's first handoff pull has drained.
func (c *Client) PullHandoffs(cursor int64, bootstrap bool) (teamwire.PullResponse, error) {
	body, err := json.Marshal(teamwire.PullRequest{SchemaVersion: teamwire.WireVersion, Cursor: cursor,
		Kinds: []string{teamwire.KindHandoff}, Bootstrap: bootstrap})
	if err != nil {
		return teamwire.PullResponse{}, err
	}
	raw, err := c.signedPost(teamwire.RoutePull, body, handoffPullReadLimit)
	if err != nil {
		return teamwire.PullResponse{}, err
	}
	var out teamwire.PullResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return teamwire.PullResponse{}, fmt.Errorf("handoff pull: %w", err)
	}
	return out, nil
}

// Members reads the organization's member directory: user id and display name only
// (OD-10), and the calling device's own user id.
func (c *Client) Members() (teamwire.MembersResponse, error) {
	body, err := json.Marshal(teamwire.MembersRequest{SchemaVersion: teamwire.WireVersion})
	if err != nil {
		return teamwire.MembersResponse{}, err
	}
	var out teamwire.MembersResponse
	err = c.do(http.MethodPost, teamwire.RouteMembers, body, &out)
	return out, err
}

// signedPost is do with a caller-chosen read bound, returning the answer's bytes. A
// response at the bound is refused rather than decoded as a truncated document.
func (c *Client) signedPost(route string, body []byte, limit int64) ([]byte, error) {
	nonce, err := teamwire.NewNonce()
	if err != nil {
		return nil, err
	}
	sr := teamwire.SignedRequest{Origin: c.origin, DeviceID: c.deviceID, Method: http.MethodPost, Route: route,
		Timestamp: c.now().Unix(), Nonce: nonce, BodySHA256: teamwire.BodyDigest(body)}
	sig, err := teamwire.Sign(c.key, sr)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.origin+route, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(teamwire.HeaderDevice, c.deviceID)
	req.Header.Set(teamwire.HeaderTimestamp, fmt.Sprint(sr.Timestamp))
	req.Header.Set(teamwire.HeaderNonce, sr.Nonce)
	req.Header.Set(teamwire.HeaderBodyDigest, sr.BodySHA256)
	req.Header.Set(teamwire.HeaderSignature, sig)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &ErrServer{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, &se.Body) // a non-JSON refusal keeps its status and an empty envelope
		return nil, se
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("the answer to %s is larger than %d bytes", route, limit)
	}
	return raw, nil
}
