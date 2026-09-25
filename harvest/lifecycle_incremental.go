package harvest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

type lifecycleRecordReader struct {
	file             *os.File
	reader           *bufio.Reader
	offset           int64
	sourceLine       int64
	bytesInspected   int64
	peakLineBytes    int64
	records          int
	maxBytes         int64
	maxRecords       int
	continuation     bool
	path             string
	runtime          string
	sourceSegmentID  string
	sourceGeneration string
	discard          *lifecycleDiscardState
	oversized        []OversizedRecordObservation
}

func newLifecycleRecordReader(request LifecycleReadRequest, discard *lifecycleDiscardState) (*lifecycleRecordReader, error) {
	if request.Path == "" || request.Offset < 0 || request.SourceLine < 0 {
		return nil, fmt.Errorf("invalid lifecycle read boundary")
	}
	maxBytes := request.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultLifecycleReadBytes
	}
	maxRecords := request.MaxRecords
	if maxRecords <= 0 {
		maxRecords = DefaultLifecycleReadRecords
	}
	f, err := os.Open(request.Path)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(request.Offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	if discard != nil && (discard.RecordStart < 0 || discard.DiscardedThrough != request.Offset || discard.IssueID == "") {
		_ = f.Close()
		return nil, fmt.Errorf("invalid oversized-record discard state")
	}
	return &lifecycleRecordReader{file: f, reader: bufio.NewReaderSize(f, lifecycleReaderBufferBytes),
		offset: request.Offset, sourceLine: request.SourceLine, maxBytes: maxBytes,
		maxRecords: maxRecords, path: request.Path, runtime: request.Runtime,
		sourceSegmentID: request.SourceSegmentID, sourceGeneration: request.SourceGeneration,
		discard: discard}, nil
}

func (r *lifecycleRecordReader) Close() error { return r.file.Close() }

// Next returns newline-terminated JSONL records only. A trailing partial record is
// inspected but never committed: offset/sourceLine remain at its beginning, making a
// crash or later append a safe bounded suffix replay.
func (r *lifecycleRecordReader) oversizedIssueID(recordStart int64) string {
	key := r.runtime + "\x00" + r.path + "\x00" + r.sourceSegmentID + "\x00" +
		r.sourceGeneration + "\x00" + fmt.Sprintf("%d", recordStart)
	return "oversized_" + lifecycleDigest([]byte(key))[len("sha256-v1:"):]
}

func (r *lifecycleRecordReader) noteOversized() {
	if r.discard == nil {
		return
	}
	for index := range r.oversized {
		if r.oversized[index].IssueID == r.discard.IssueID {
			r.oversized[index].DiscardedThrough = r.discard.DiscardedThrough
			return
		}
	}
	r.oversized = append(r.oversized, OversizedRecordObservation{IssueID: r.discard.IssueID,
		RecordStart: r.discard.RecordStart, DiscardedThrough: r.discard.DiscardedThrough,
		SourceGeneration: r.discard.SourceGeneration})
}

func (r *lifecycleRecordReader) discardNext() (bool, error) {
	for {
		if r.bytesInspected >= r.maxBytes {
			r.continuation = true
			r.noteOversized()
			return false, nil
		}
		fragment, err := r.reader.ReadSlice('\n')
		r.bytesInspected += int64(len(fragment))
		r.offset += int64(len(fragment))
		r.discard.DiscardedThrough = r.offset
		r.noteOversized()
		switch {
		case err == nil:
			r.sourceLine++
			r.discard = nil
			return true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return false, nil
		default:
			return false, err
		}
	}
}

func (r *lifecycleRecordReader) Next() ([]byte, bool, error) {
	for {
		if r.discard != nil {
			complete, err := r.discardNext()
			if err != nil || !complete {
				return nil, false, err
			}
		}
		if r.records >= r.maxRecords || r.bytesInspected >= r.maxBytes {
			r.continuation = true
			return nil, false, nil
		}
		recordStart := r.offset
		var line bytes.Buffer
		consumed := int64(0)
		for {
			fragment, err := r.reader.ReadSlice('\n')
			consumed += int64(len(fragment))
			r.bytesInspected += int64(len(fragment))
			if int64(line.Len())+int64(len(fragment)) > MaxLifecycleRecordBytes {
				r.offset = recordStart + consumed
				r.discard = &lifecycleDiscardState{IssueID: r.oversizedIssueID(recordStart),
					RecordStart: recordStart, DiscardedThrough: r.offset,
					SourceGeneration: r.sourceGeneration}
				r.noteOversized()
				switch {
				case err == nil:
					r.sourceLine++
					r.discard = nil
				case errors.Is(err, bufio.ErrBufferFull):
					complete, discardErr := r.discardNext()
					if discardErr != nil || !complete {
						return nil, false, discardErr
					}
				case errors.Is(err, io.EOF):
					return nil, false, nil
				default:
					return nil, false, err
				}
				break
			}
			_, _ = line.Write(fragment)
			if retained := int64(cap(line.Bytes())); retained > r.peakLineBytes {
				r.peakLineBytes = retained
			}
			switch {
			case err == nil:
				r.offset = recordStart + consumed
				r.sourceLine++
				r.records++
				body := line.Bytes()
				return append([]byte(nil), body[:len(body)-1]...), true, nil
			case errors.Is(err, bufio.ErrBufferFull):
				continue
			case errors.Is(err, io.EOF):
				r.offset = recordStart
				return nil, false, nil
			default:
				return nil, false, err
			}
		}
	}
}

func (r *lifecycleRecordReader) finish(batch *LifecycleBatch) error {
	batch.NextOffset = r.offset
	batch.NextSourceLine = r.sourceLine
	batch.BytesInspected = r.bytesInspected
	batch.PeakRetainedBytes = r.peakLineBytes + int64(r.reader.Size()) + int64(len(batch.ParserState))
	batch.RecordsDecoded = r.records
	batch.OversizedRecords = append([]OversizedRecordObservation(nil), r.oversized...)
	if !r.continuation {
		info, err := r.file.Stat()
		if err != nil {
			return err
		}
		r.continuation = r.offset < info.Size() && r.records > 0
	}
	batch.Continuation = r.continuation
	return nil
}

type lifecycleCallState struct {
	Tool    string            `json:"tool,omitempty"`
	Session string            `json:"session,omitempty"`
	Effects []LifecycleEffect `json:"effects,omitempty"`
}

type lifecycleParserState struct {
	Version   int                           `json:"version,omitempty"`
	Canonical string                        `json:"canonical,omitempty"`
	Calls     map[string]lifecycleCallState `json:"calls,omitempty"`
	Order     []string                      `json:"order,omitempty"`
	Discard   *lifecycleDiscardState        `json:"discard,omitempty"`
}

type lifecycleDiscardState struct {
	IssueID          string `json:"issue_id"`
	RecordStart      int64  `json:"record_start"`
	DiscardedThrough int64  `json:"discarded_through"`
	SourceGeneration string `json:"source_generation,omitempty"`
}

func decodeLifecycleState(body []byte) (lifecycleParserState, error) {
	state := lifecycleParserState{Calls: map[string]lifecycleCallState{}}
	if len(body) == 0 {
		return state, nil
	}
	if len(body) > MaxLifecycleStateBytes {
		return state, fmt.Errorf("lifecycle parser state exceeds %d bytes", MaxLifecycleStateBytes)
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return state, fmt.Errorf("decode lifecycle parser state: %w", err)
	}
	if state.Calls == nil {
		state.Calls = map[string]lifecycleCallState{}
	}
	if state.Discard != nil && (state.Discard.IssueID == "" || state.Discard.RecordStart < 0 ||
		state.Discard.DiscardedThrough < state.Discard.RecordStart) {
		return state, fmt.Errorf("invalid oversized-record discard state")
	}
	return state, nil
}

func rememberLifecycleCall(state *lifecycleParserState, id string, call lifecycleCallState) {
	if id == "" {
		return
	}
	if _, found := state.Calls[id]; !found {
		state.Order = append(state.Order, id)
	}
	state.Calls[id] = call
}

func forgetLifecycleCall(state *lifecycleParserState, id string) {
	delete(state.Calls, id)
	for index := range state.Order {
		if state.Order[index] == id {
			state.Order = append(state.Order[:index], state.Order[index+1:]...)
			return
		}
	}
}

func boundedLifecycleState(calls map[string]lifecycleCallState, order []string, canonical string,
	discard *lifecycleDiscardState) ([]byte, []string, []string, error) {
	evicted := []string{}
	for {
		body, err := jsonMarshal(lifecycleParserState{Version: 1, Canonical: canonical, Calls: calls,
			Order: order, Discard: discard})
		if err != nil {
			return nil, nil, nil, err
		}
		if len(body) <= MaxLifecycleStateBytes || len(order) == 0 {
			return body, order, evicted, nil
		}
		evicted = append(evicted, order[0])
		delete(calls, order[0])
		order = order[1:]
	}
}

// jsonMarshal is a variable only so focused tests can exercise state-bound failures
// without replacing filesystem or parser owners.
var jsonMarshal = func(v any) ([]byte, error) {
	return json.Marshal(v)
}
