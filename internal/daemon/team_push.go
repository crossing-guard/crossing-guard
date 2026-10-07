package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"crossing-guard/engine"
	"crossing-guard/internal/teamlink"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// The outbox drain (team item 4, plan §4.2 decision 1). Its own goroutine and cadence
// from team.json, keyed on the link's lifecycle like the report and pull jobs; never on
// the hot path (invariant 5). Each tick pushes one batch of outbox rows, oldest first,
// plus a session projection for every session the batch touched, and acknowledges each
// row by its per-record answer:
//
//   - accepted, duplicate: acknowledged.
//   - rejected (any code but unsupported_kind): acknowledged terminally and counted per
//     kind and code — a record refused on its merits is never retried.
//   - conflict: acknowledged terminally, counted separately (a security event server-side).
//   - rejected with internal (the server failed to store it): NOT acknowledged — it is
//     retried next tick, and dead-lettered as internal_exhausted after push_max_attempts
//     so a row the server can never store cannot hold the head forever.
//   - a handoff or a handoff receipt (team rest-of-release plan §6.6): its answer settles
//     through the store's handoff transition — the acknowledgement and the handoff's
//     state in one transaction. These two kinds are read ahead of every other kind.
//   - rejected with unsupported_kind: NOT acknowledged. The kind is parked — left out of
//     every batch, so it can never occupy the batch head — and re-probed with one row per
//     parked_kind_retry. Acknowledging it would lose the rows for good (N2).
//
// A transport failure acknowledges nothing; the batch is retried on the next tick.

// pushKindsWithoutEncoder are outbox kinds this build cannot yet put on the wire. They are
// parked without a probe: nothing is lost, the console names them. Item 5 built the memory
// and tombstone encoders (behind decision 14's gate, in store.MemoryPushRecord), so none
// remain; the mechanism stays for the next kind.
var pushKindsWithoutEncoder = map[string]bool{}

// memorySyncKind reports the outbox kinds whose answers settle through the store's memory
// sync transition (team item 5 decision 13b), not a bare acknowledgement.
func memorySyncKind(kind string) bool {
	return kind == store.OutboxMemory || kind == store.OutboxTombstone
}

// handoffKind reports the outbox kinds whose answers settle through the store's handoff
// transition (team rest-of-release plan §6.6): the acknowledgement and the handoff's
// state in one transaction, never a bare acknowledgement.
func handoffKind(kind string) bool {
	return kind == store.OutboxHandoff || kind == store.OutboxHandoffReceipt
}

func (t *teamLinker) startPushLocked() {
	if t.pushStop != nil {
		return
	}
	stop := make(chan struct{})
	t.pushStop = stop
	t.jobs.Go(func() { t.runPush(stop) })
	// Handoff transport rides the drain's lifecycle: the handoff pull and the member
	// directory refresh start and stop with it (team_handoff_sync.go).
	t.jobs.Go(func() { t.runHandoffSync(stop) })
}

// pushSoon drains one batch now, as one of the linker's jobs, instead of waiting for
// the cadence: a handoff a person just sent, withdrew, declined or closed leaves at
// once. It does nothing when the device is not linked or the linker is stopping.
func (t *teamLinker) pushSoon() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.halted || t.state != teamLinked {
		return
	}
	t.jobs.Go(t.pushOnce)
}

func (t *teamLinker) runPush(stop chan struct{}) {
	t.mu.Lock()
	interval := t.doc.PushInterval.Duration
	t.mu.Unlock()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		t.pushOnce()
		timer.Reset(interval)
	}
}

// pushBatch is one tick's records and the outbox rows each answer settles.
type pushBatch struct {
	records []teamwire.PushRecord
	rows    map[string][]store.OutboxRow // record id → the rows it carries
	kinds   map[string]string            // record id → kind
	local   []localRefusal               // rows refused before leaving the device
	// contentBytes: a content record id → its session and size, counted against the
	// per-session cap once the server accepts it.
	contentBytes map[string]contentSize
}

type contentSize struct {
	session  string
	bytes    int64
	bodyHash string // the redacted body's hash as sent, recorded with an accepting answer (C-3)
}

type localRefusal struct {
	row  store.OutboxRow
	code string
}

// pushOnce drains one batch. The push runs without t.mu; the link generation captured
// under the lock drops a result that lands after an unlink or a relink.
func (t *teamLinker) pushOnce() {
	t.pushMu.Lock() // one tick at a time: a batch is settled before the next is read
	defer t.pushMu.Unlock()
	t.mu.Lock()
	if t.state != teamLinked || t.key == nil {
		t.mu.Unlock()
		return
	}
	if t.driftCheck() == driftChanged { // a repointed team.json silences the drain too
		t.state, t.problem = teamDrift, "team.json or the device key changed outside the daemon; reporting stopped — unlink and link again"
		t.mu.Unlock()
		t.linkEvent("team.drift", t.problem)
		return
	}
	gen, now := t.gen, t.now()
	batchSize, retry, maxAttempts, maxBytes := t.doc.PushBatch, t.doc.ParkedKindRetry.Duration, t.doc.PushMaxAttempts, t.doc.PushMaxBytes
	if t.fitBytes > 0 && t.fitBytes < maxBytes {
		maxBytes = t.fitBytes // the server answered 413 below the configured cap
	}
	client, clientErr := teamlink.NewClient(t.doc.Server, t.deviceID, t.key, t.doc.RequestTimeout.Duration, t.now)
	skip, probes := t.parkedKindsLocked(now)
	_, sessionsParked := t.parked[teamwire.KindSession]
	sessionsParked = sessionsParked && !slices.Contains(probes, teamwire.KindSession)
	sessionCap, orgID := t.doc.ContentSessionCap, t.doc.Organization.ID
	sent := copyBytes(t.report.Push.ContentBytes)
	t.mu.Unlock()
	// Read after the lock is released: the adoption records are a file with its own lock.
	mandate := adoptedContentMandate(t.storeDir, orgID, now)
	if clientErr != nil {
		t.recordPush(gen, "error", clientErr.Error(), nil)
		return
	}
	if err := syncContentMandate(mandate, now); err != nil {
		log.Printf("team push: the content mandate could not be recorded: %v", err)
	}
	rows, err := governor.ix.OutboxBatch(batchSize, skip)
	if err != nil {
		t.recordPush(gen, "error", "reading the outbox: "+err.Error(), nil)
		return
	}
	for _, kind := range probes {
		row, ok, err := governor.ix.OutboxOldestOfKind(kind)
		if err != nil {
			t.recordPush(gen, "error", "reading the outbox: "+err.Error(), nil)
			return
		}
		if ok {
			rows = append(rows, row)
		}
		t.mu.Lock()
		if t.gen == gen {
			t.parked[kind] = now.Add(retry) // one probe per retry interval, answered or not
		}
		t.mu.Unlock()
	}
	if len(rows) == 0 {
		return
	}
	policy, err := loadContentPolicy(mandate, sessionCap, sent)
	if err != nil {
		t.recordPush(gen, "error", "reading content consent: "+err.Error(), nil)
		return
	}
	b, err := buildPushBatch(rows, now, policy, sessionsParked, orgID)
	if err != nil {
		t.recordPush(gen, "error", err.Error(), nil)
		return
	}
	b = b.fit(maxBytes, policy)
	var res teamwire.PushResponse
	if len(b.records) > 0 {
		res, err = client.Push(b.records)
		if err != nil {
			if teamlink.Code(err) == teamwire.CodeDeviceRevoked {
				t.mu.Lock()
				if t.gen == gen && t.state == teamLinked {
					t.state, t.problem = teamRevoked, "the server revoked this device's key; sync is paused and nothing local is deleted — unlink and link again to re-enroll"
				}
				t.mu.Unlock()
				t.recordPush(gen, "revoked", err.Error(), b.local)
				return
			}
			var se *teamlink.ErrServer
			if errors.As(err, &se) && se.Status == http.StatusRequestEntityTooLarge {
				// The server's request cap is below push_max_bytes (a hand-mirrored value
				// team.json cannot change after linking): halve the fitted size, and
				// refuse a lone record by name, so no head retries forever.
				t.mu.Lock()
				if t.gen == gen {
					t.fitBytes = max(maxBytes/2, teamlink.MinPushBytes)
				}
				t.mu.Unlock()
				if outbox := b.outboxRecords(); len(outbox) == 1 || maxBytes <= teamlink.MinPushBytes {
					for _, id := range outbox {
						for _, row := range b.rows[id] {
							b.local = append(b.local, localRefusal{row, "over_push_limit"})
						}
					}
				}
			}
			t.recordPush(gen, "error", err.Error(), b.local)
			return
		}
	}
	t.settlePush(gen, now, retry, maxAttempts, b, res)
}

// parkedKindsLocked splits the parked kinds into those to leave out of the batch and
// those due a probe. Kinds with no encoder in this build are always left out.
func (t *teamLinker) parkedKindsLocked(now time.Time) (skip, probes []string) {
	if t.parked == nil {
		t.parked = map[string]time.Time{}
	}
	for kind := range pushKindsWithoutEncoder {
		skip = append(skip, kind)
	}
	for kind, due := range t.parked {
		if pushKindsWithoutEncoder[kind] {
			continue
		}
		skip = append(skip, kind)
		if !now.Before(due) {
			probes = append(probes, kind)
		}
	}
	sort.Strings(skip)
	sort.Strings(probes)
	return skip, probes
}

// settlePush applies each answer to its rows, then persists the counters. Every
// acknowledgement records the answer it was acknowledged with (C-3); a memory or
// tombstone row settles through the store's one sync transition instead (decision 13b),
// carrying the server revision or, on stale_base, the revision the server holds.
func (t *teamLinker) settlePush(gen uint64, now time.Time, retry time.Duration, maxAttempts int, b pushBatch, res teamwire.PushResponse) {
	var acks []store.OutboxAckEntry
	var settles []store.MemorySettle
	var handoffs []store.HandoffSettle
	ack := func(row store.OutboxRow, code string, answer *teamwire.MemoryAnswer, bodyHash string) {
		if handoffKind(row.Kind) {
			handoffs = append(handoffs, store.HandoffSettle{Seq: row.Seq, Code: code})
			return
		}
		if memorySyncKind(row.Kind) {
			s := store.MemorySettle{Seq: row.Seq, Code: code}
			if answer != nil {
				s.ServerRevision, s.Current = answer.ServerRevision, answer.Current
			}
			settles = append(settles, s)
			return
		}
		acks = append(acks, store.OutboxAckEntry{Seq: row.Seq, Code: code, SentBodyHash: bodyHash})
	}
	for _, l := range b.local {
		ack(l.row, l.code, nil, "")
	}
	var internal []int64
	answered := map[string]teamwire.PushResult{}
	for _, r := range res.Results {
		answered[r.ID] = r
	}
	t.mu.Lock()
	if t.gen != gen {
		t.mu.Unlock()
		return
	}
	st := &t.report.Push
	for _, l := range b.local {
		countRefusal(st, l.row.Kind, l.code, false)
	}
	for _, rec := range b.records {
		r, ok := answered[rec.ID]
		kind := b.kinds[rec.ID]
		if !ok {
			continue // unanswered: the rows stay pending and ride the next tick
		}
		switch r.Status {
		case teamwire.StatusAccepted, teamwire.StatusDuplicate:
			c, isContent := b.contentBytes[rec.ID]
			if r.Status == teamwire.StatusAccepted {
				st.Accepted++
				if isContent {
					if st.ContentBytes == nil {
						st.ContentBytes = map[string]int64{}
					}
					st.ContentBytes[c.session] += c.bytes
				}
			}
			delete(t.parked, kind) // an accepting answer un-parks a probed kind
			for _, row := range b.rows[rec.ID] {
				ack(row, r.Status, r.Memory, c.bodyHash)
			}
		case teamwire.StatusRejected:
			code := "rejected"
			if r.Error != nil && r.Error.Code != "" {
				code = r.Error.Code
			}
			if code == teamwire.CodeInternal {
				// Only an answered internal counts toward the cap: transport failures and
				// parked-kind probes never spend it (postwork C8).
				for _, row := range b.rows[rec.ID] {
					internal = append(internal, row.Seq)
					if row.Attempts+1 >= maxAttempts {
						countRefusal(st, kind, "internal_exhausted", true)
						ack(row, "internal_exhausted", nil, "")
					}
				}
				continue
			}
			if code == teamwire.CodeUnsupportedKind {
				if _, already := t.parked[kind]; !already {
					t.parked[kind] = now.Add(retry)
					log.Printf("team push: the server does not accept %s records yet; parked, re-probed every %s", kind, retry)
				}
				continue
			}
			countRefusal(st, kind, code, true)
			for _, row := range b.rows[rec.ID] {
				ack(row, code, r.Memory, "")
			}
		case teamwire.StatusConflict:
			if st.Conflicts == nil {
				st.Conflicts = map[string]int{}
			}
			st.Conflicts[kind]++
			log.Printf("team push: %s %s conflicts with what the server holds (same id, different content); acknowledged, not retried", kind, rec.ID)
			for _, row := range b.rows[rec.ID] {
				ack(row, teamwire.StatusConflict, nil, "")
			}
		}
	}
	st.LastAt, st.Outcome, st.Error = now, "pushed", ""
	if err := teamlink.SaveReportState(t.storeDir, t.report); err != nil {
		log.Printf("team push: state not written: %v", err)
	}
	t.mu.Unlock()
	if err := governor.ix.OutboxAttempted(internal); err != nil {
		log.Printf("team push: attempts not counted: %v", err)
	}
	if err := governor.ix.OutboxAckEntries(acks, now.Unix()); err != nil {
		log.Printf("team push: acknowledgement not written (the rows re-push and dedupe): %v", err)
	}
	t.settleMemory(settles, now)
	t.settleHandoffs(handoffs, now)
}

// settleMemory applies each memory or tombstone answer in its own store transaction and
// re-mirrors what landed (a stale_base's carried revision, a held revision).
func (t *teamLinker) settleMemory(settles []store.MemorySettle, now time.Time) {
	for _, s := range settles {
		eff, err := governor.ix.SettleMemoryPush(s, now.Unix())
		if err != nil {
			log.Printf("team push: memory answer for row %d not settled (it re-pushes; the server dedupes): %v", s.Seq, err)
			continue
		}
		mirrorMemoryEffects(eff)
	}
}

// countRefusal counts one terminal refusal by kind and code — in DeadLetter when the
// server refused it, in Refused when this device did (consent revoked, over a cap, not
// encodable): the console must not say "the server refused" for what it never saw.
func countRefusal(st *teamlink.PushState, kind, code string, byServer bool) {
	m := &st.Refused
	if byServer {
		m = &st.DeadLetter
	}
	if *m == nil {
		*m = map[string]map[string]int{}
	}
	if (*m)[kind] == nil {
		(*m)[kind] = map[string]int{}
	}
	(*m)[kind][code]++
}

// recordPush persists a failed tick's outcome; local refusals are still settled, since
// they never depended on the server.
func (t *teamLinker) recordPush(gen uint64, outcome, detail string, local []localRefusal) {
	t.mu.Lock()
	if t.gen != gen {
		t.mu.Unlock()
		return
	}
	t.report.Push.LastAt, t.report.Push.Outcome, t.report.Push.Error = t.now(), outcome, detail
	var acks []store.OutboxAckEntry
	var settles []store.MemorySettle
	var handoffs []store.HandoffSettle
	for _, l := range local {
		countRefusal(&t.report.Push, l.row.Kind, l.code, false)
		if handoffKind(l.row.Kind) {
			handoffs = append(handoffs, store.HandoffSettle{Seq: l.row.Seq, Code: l.code})
		} else if memorySyncKind(l.row.Kind) {
			settles = append(settles, store.MemorySettle{Seq: l.row.Seq, Code: l.code})
		} else {
			acks = append(acks, store.OutboxAckEntry{Seq: l.row.Seq, Code: l.code})
		}
	}
	if err := teamlink.SaveReportState(t.storeDir, t.report); err != nil {
		log.Printf("team push: state not written: %v", err)
	}
	t.mu.Unlock()
	log.Printf("team push: %s: %s", outcome, detail)
	if err := governor.ix.OutboxAckEntries(acks, t.now().Unix()); err != nil {
		log.Printf("team push: acknowledgement not written: %v", err)
	}
	t.settleMemory(settles, t.now())
	t.settleHandoffs(handoffs, t.now())
}

// buildPushBatch turns outbox rows into wire records: each event encoded by the one wire
// encoder (redacted against the shipped secret patterns — invariant 7), each checkpoint
// fact read from its terminal row, then one session projection per session the batch
// touched. A row whose source cannot be encoded is refused locally with a named code.
func buildPushBatch(rows []store.OutboxRow, now time.Time, policy contentPolicy, sessionsParked bool, orgID string) (pushBatch, error) {
	b := pushBatch{rows: map[string][]store.OutboxRow{}, kinds: map[string]string{}, contentBytes: map[string]contentSize{}}
	dets, err := engine.DefaultDetectors()
	if err != nil {
		return b, fmt.Errorf("loading the secret patterns: %w", err)
	}
	deviceID, _, err := governor.ix.Device()
	if err != nil {
		return b, err
	}
	var eventIDs []string
	for _, r := range rows {
		if r.Kind == store.OutboxEvent {
			eventIDs = append(eventIDs, r.GlobalID)
		}
	}
	inputs, err := governor.ix.EventWireInputsByGlobalID(eventIDs)
	if err != nil {
		return b, err
	}
	sessions := map[string]engine.WireSession{} // local session id → its wire identity
	var sessionOrder []string
	add := func(kind, id string, body []byte, row store.OutboxRow) {
		if _, seen := b.kinds[id]; !seen {
			b.records = append(b.records, teamwire.PushRecord{Kind: kind, ID: id, ContentHash: teamwire.ContentHash(body), Body: body})
			b.kinds[id] = kind
		}
		b.rows[id] = append(b.rows[id], row)
	}
	for _, r := range rows {
		switch r.Kind {
		case store.OutboxEvent:
			in, ok := inputs[r.GlobalID]
			if !ok {
				b.local = append(b.local, localRefusal{r, "source_missing"})
				continue
			}
			body, err := engine.EncodeWireEvent(in, dets)
			if err != nil {
				b.local = append(b.local, localRefusal{r, "encode_failed"})
				log.Printf("team push: event %s not encodable: %v", r.GlobalID, err)
				continue
			}
			add(teamwire.KindEvent, r.GlobalID, body, r)
			if _, seen := sessions[in.Session]; !seen && !sessionsParked {
				var ev engine.WireEvent
				if err := json.Unmarshal(body, &ev); err != nil {
					return b, fmt.Errorf("re-reading an encoded event: %w", err)
				}
				sessions[in.Session] = ev.Session
				sessionOrder = append(sessionOrder, in.Session)
			}
		case store.OutboxCheckpointFact:
			body, ok, err := checkpointFactBody(deviceID, r)
			if err != nil {
				return b, err
			}
			if !ok {
				b.local = append(b.local, localRefusal{r, "source_missing"})
				continue
			}
			add(teamwire.KindCheckpointFact, r.GlobalID, body, r)
		case store.OutboxContent:
			body, code, err := contentChunkBody(deviceID, r, policy, dets)
			if err != nil {
				return b, err
			}
			if code != "" {
				b.local = append(b.local, localRefusal{r, code})
				continue
			}
			add(teamwire.KindSessionContent, r.GlobalID, body, r)
			var chunk teamwire.ContentChunk
			if err := json.Unmarshal(body, &chunk); err != nil {
				return b, fmt.Errorf("re-reading an encoded chunk: %w", err)
			}
			b.contentBytes[r.GlobalID] = contentSize{chunk.SessionID, int64(len(chunk.Body)), chunk.BodyHash}
		case store.OutboxMemory, store.OutboxTombstone:
			// Decision 14's gate runs again here, on current state; the queued revision is
			// encoded (never current state) and frozen before the request leaves.
			rec, code, err := governor.ix.MemoryPushRecord(r, dets, orgID)
			if err != nil {
				return b, err
			}
			if code != "" {
				b.local = append(b.local, localRefusal{r, code})
				continue
			}
			add(rec.Kind, rec.ID, rec.Body, r)
		case store.OutboxHandoff, store.OutboxHandoffReceipt:
			// The body was frozen in the transaction that queued the row: what leaves is
			// those bytes, on the first push and on every retry (§6.6).
			if r.SentWireBody == "" {
				b.local = append(b.local, localRefusal{r, "source_missing"})
				continue
			}
			add(r.Kind, r.GlobalID, []byte(r.SentWireBody), r)
		default:
			b.local = append(b.local, localRefusal{r, "unknown_kind"})
		}
	}
	for _, local := range sessionOrder {
		body, err := sessionRecordBody(deviceID, local, sessions[local], now)
		if err != nil {
			return b, err
		}
		id := sessions[local].ID
		if _, seen := b.kinds[id]; !seen {
			b.records = append(b.records, teamwire.PushRecord{Kind: teamwire.KindSession, ID: id, ContentHash: teamwire.ContentHash(body), Body: body})
			b.kinds[id] = teamwire.KindSession
		}
	}
	return b, nil
}

func rfc3339(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }

// sessionRecordBody is the session projection: identities from the encoded event, the
// event span from the store, and this device's own chain verification — a claim.
func sessionRecordBody(deviceID, localSession string, ws engine.WireSession, now time.Time) ([]byte, error) {
	report, err := governor.ChainVerify(localSession)
	if err != nil {
		return nil, err
	}
	count, first, last, err := governor.ix.SessionEventSpan(localSession)
	if err != nil {
		return nil, err
	}
	rec := teamwire.SessionRecord{SchemaVersion: "1.0", ID: ws.ID, DeviceID: deviceID,
		Session: teamwire.SessionIdentity{ID: ws.ID, Runtime: ws.Runtime, NativeID: ws.NativeID, CatalogID: ws.CatalogID, ResumeID: ws.ResumeID},
		Events:  teamwire.EventSpan{Count: count},
		ChainClaim: teamwire.ChainClaim{Status: report.Status, Rows: report.Rows, Chained: report.Chained, Legacy: report.Legacy,
			DiskSpan: report.DiskSpan, TailMatchesHeld: report.TailMatchesHeld},
		ReportedAt: now.UTC().Format(time.RFC3339)}
	if report.HeldSpan != [2]int64{} {
		h := report.HeldSpan
		rec.ChainClaim.HeldSpan = &h
	}
	if count > 0 {
		f, l := rfc3339(first), rfc3339(last)
		rec.Events.FirstAt, rec.Events.LastAt = &f, &l
	}
	if repo, _, ok, err := governor.ix.SessionRepository(localSession); err != nil {
		return nil, err
	} else if ok {
		rec.Workspace.RepositoryID = &repo
	}
	return json.Marshal(rec)
}

// checkpointFactBody reads the terminal checkpoint the row names and renders its facts.
// ok is false when none matches.
func checkpointFactBody(deviceID string, r store.OutboxRow) ([]byte, bool, error) {
	c, ok, err := governor.ix.CheckpointForFact(deviceID, r.Scope, r.GlobalID)
	if err != nil || !ok {
		return nil, false, err
	}
	fact := teamwire.CheckpointFact{SchemaVersion: "1.0", ID: r.GlobalID, DeviceID: deviceID,
		SessionID: checkpointSessionWireID(deviceID, c.Runtime, c.SessionID),
		Kind:      c.Kind, Status: c.Status, BoundaryClass: c.BoundaryClass, RequestedAt: rfc3339(store.UnixSeconds(c.RequestedAt)),
		FailureKind: c.FailureKind, DetailDigest: c.DetailDigest}
	if c.RepositoryID != "" {
		repo := c.RepositoryID
		fact.RepositoryID = &repo
	}
	if c.CaptureEndedAt != 0 {
		e := rfc3339(store.UnixSeconds(c.CaptureEndedAt))
		fact.EndedAt = &e
	}
	if c.ChangeRecordID != 0 {
		cr, err := governor.ix.ChangeRecordByID(c.ChangeRecordID)
		if err != nil {
			return nil, false, err
		}
		fact.Revision = &teamwire.CheckpointRevision{Kind: cr.Kind, EvidenceClass: cr.EvidenceClass,
			Base: optional(cr.BaseRevision), Head: optional(cr.HeadRevision), SnapshotDigest: optional(cr.SnapshotDigest),
			SourceDigest: cr.SourceDigest, VerificationResult: optional(cr.VerificationResult), Items: len(cr.Items)}
	}
	body, err := json.Marshal(fact)
	return body, err == nil, err
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func checkpointSessionWireID(deviceID, runtimeColumn, session string) string {
	runtime, native := engine.WireSessionParts(runtimeColumn, session)
	return engine.WireSessionID(deviceID, runtime+"/"+native)
}

// contentMandate is an adopted bundle's content policy — organization or repository
// scope, read from the adopted-bundle record and from nothing in an offer (D-12's
// second path; team rest-of-release plan OD-27):
// when it says "mandated", every session in scope sends content under consent
// "mandated-by-bundle", and the console says which bundle.
type contentMandate struct {
	BundleID     string   `json:"bundle_id"`
	Repositories []string `json:"repositories"` // empty = every repository
}

// syncContentMandate keeps the store's mandate row in step with the adopted bundles, so
// the enqueue (inside the observe transaction) sees it without asking the daemon.
func syncContentMandate(m *contentMandate, now time.Time) error {
	return governor.ix.SetContentOptIn(store.ContentOptInAll, m != nil, now.Unix())
}

// contentPolicy is what the drain decides content by: the recorded consents, the
// mandate, and the per-session cap with what each session has already sent.
type contentPolicy struct {
	optIns  map[string]int64
	mandate *contentMandate
	cap     int64
	sent    map[string]int64 // wire session id → accepted content bytes
}

func loadContentPolicy(m *contentMandate, sessionCap int64, sent map[string]int64) (contentPolicy, error) {
	optIns, err := governor.ix.ContentOptIns()
	return contentPolicy{optIns: optIns, mandate: m, cap: sessionCap, sent: sent}, err
}

func copyBytes(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// contentChunkBody renders one content row, or names why it cannot leave: consent
// revoked since it was enqueued, outside the mandate's repositories, not text, over the
// chunk cap, or over the session's cap. The body is redacted against the shipped
// secret patterns first (invariant 7); the hash is of what is sent.
func contentChunkBody(deviceID string, r store.OutboxRow, p contentPolicy, dets []engine.Detector) ([]byte, string, error) {
	in, ok, err := governor.ix.ContentInputByChunk(r.Scope, r.GlobalID)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "source_missing", nil
	}
	consent := ""
	if _, ok := p.optIns[store.ContentSessionKey(in.Runtime, in.Session)]; ok {
		consent = teamlink.ContentConsent
	} else if _, ok := p.optIns[store.ContentOptInAll]; ok && p.mandate != nil {
		if len(p.mandate.Repositories) > 0 {
			repo, _, _, err := governor.ix.SessionRepository(in.Session)
			if err != nil {
				return nil, "", err
			}
			if !slices.Contains(p.mandate.Repositories, repo) {
				return nil, "outside_mandate", nil
			}
		}
		consent = teamlink.ContentMandated
	}
	if consent == "" {
		return nil, "consent_revoked", nil
	}
	if !utf8.Valid(in.Payload) {
		return nil, "not_text", nil
	}
	text, fired := engine.RedactText(string(in.Payload), dets)
	if len(text) > teamwire.ContentChunkMaxBytes {
		return nil, "over_chunk_cap", nil
	}
	sessionID := engine.WireSessionID(deviceID, store.ContentSessionKey(in.Runtime, in.Session))
	if p.sent[sessionID]+int64(len(text)) > p.cap {
		return nil, "over_session_cap", nil
	}
	sum := sha256.Sum256([]byte(text))
	if fired == nil {
		fired = []string{}
	}
	body, err := json.Marshal(teamwire.ContentChunk{SchemaVersion: "1.0", ID: r.GlobalID, DeviceID: deviceID, SessionID: sessionID,
		EventID: in.EventGlobalID, Part: teamwire.ContentPartEventInput, Consent: consent, Body: text,
		BodyHash: "sha256:" + hex.EncodeToString(sum[:]), Redactions: fired})
	return body, "", err
}

// pushEnvelopeBytes is a generous allowance for a record's JSON framing (kind, id, hash)
// on top of its body, so a fitted batch stays under the server's request cap.
const pushEnvelopeBytes = 512

// fit keeps a batch under maxBytes (the server's devices.push_max_bytes, mirrored in
// team.json): session projections always ride (they are small and not outbox rows),
// then records in outbox order while they fit. A record that cannot fit even alone is
// refused locally as over_push_limit — otherwise the whole push would be refused on
// every tick, the wedge decision 1 forbids. Rows left out stay pending for the next tick.
func (b pushBatch) fit(maxBytes int64, policy contentPolicy) pushBatch {
	total := int64(len(`{"schema_version":"1.0","records":[]}`))
	for _, r := range b.records {
		if r.Kind == teamwire.KindSession {
			total += int64(len(r.Body)) + pushEnvelopeBytes
		}
	}
	out := pushBatch{rows: map[string][]store.OutboxRow{}, kinds: map[string]string{}, contentBytes: b.contentBytes, local: b.local}
	inBatch := map[string]int64{} // content bytes this batch adds per session
	for _, r := range b.records {
		size := int64(len(r.Body)) + pushEnvelopeBytes
		if c, ok := b.contentBytes[r.ID]; ok {
			// The session cap is charged here, after sizing, against what the server
			// accepted plus what this batch carries: a chunk that would pass it waits a
			// tick (the accepted total then decides), never refused on a guess (C9).
			if policy.sent[c.session]+inBatch[c.session]+c.bytes > policy.cap {
				continue
			}
		}
		if r.Kind != teamwire.KindSession {
			if size+int64(len(`{"schema_version":"1.0","records":[]}`)) > maxBytes {
				for _, row := range b.rows[r.ID] {
					out.local = append(out.local, localRefusal{row, "over_push_limit"})
				}
				continue
			}
			if total+size > maxBytes {
				continue // waits for the next tick
			}
			total += size
			if c, ok := b.contentBytes[r.ID]; ok {
				inBatch[c.session] += c.bytes
			}
		}
		out.records = append(out.records, r)
		out.kinds[r.ID] = b.kinds[r.ID]
		if rows, ok := b.rows[r.ID]; ok {
			out.rows[r.ID] = rows
		}
	}
	return out
}

// outboxRecords lists the batch's records that settle outbox rows (not projections).
func (b pushBatch) outboxRecords() []string {
	var ids []string
	for _, r := range b.records {
		if _, ok := b.rows[r.ID]; ok {
			ids = append(ids, r.ID)
		}
	}
	return ids
}
