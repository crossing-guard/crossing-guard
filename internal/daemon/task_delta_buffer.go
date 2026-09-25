package daemon

import (
	"sync"
	"time"
)

const (
	taskDeltaFlushInterval = 250 * time.Millisecond
	taskDeltaFlushBytes    = 16 * 1024
)

type bufferedTaskDelta struct {
	taskID, runtime, kind string
	payload               ChatEvent
	timer                 *time.Timer
}

// TaskDeltaBuffer coalesces transient text/reasoning chunks before persistence. It
// owns no lifecycle or delivery cursor; its only invariant is a bounded time/size
// window, and flush hands one canonical draft back to the application service.
type TaskDeltaBuffer struct {
	mu      sync.Mutex
	entries map[string]*bufferedTaskDelta
	flush   func(string, string, string, ChatEvent)
}

func NewTaskDeltaBuffer(flush func(string, string, string, ChatEvent)) *TaskDeltaBuffer {
	return &TaskDeltaBuffer{entries: map[string]*bufferedTaskDelta{}, flush: flush}
}

func (b *TaskDeltaBuffer) Add(taskID, runtime, kind string, payload ChatEvent) {
	key := taskID + "\x00" + kind
	text := anyString(payload["text"])
	b.mu.Lock()
	entry := b.entries[key]
	if entry != nil && len(anyString(entry.payload["text"]))+len(text) > taskDeltaFlushBytes {
		delete(b.entries, key)
		entry.timer.Stop()
		flushed := *entry
		b.mu.Unlock()
		b.flush(flushed.taskID, flushed.runtime, flushed.kind, flushed.payload)
		b.Add(taskID, runtime, kind, payload)
		return
	}
	if entry == nil {
		copied := ChatEvent{}
		for name, value := range payload {
			copied[name] = value
		}
		entry = &bufferedTaskDelta{taskID: taskID, runtime: runtime, kind: kind, payload: copied}
		entry.timer = time.AfterFunc(taskDeltaFlushInterval, func() { b.flushKey(key) })
		b.entries[key] = entry
	} else {
		entry.payload["text"] = anyString(entry.payload["text"]) + text
	}
	b.mu.Unlock()
}

func (b *TaskDeltaBuffer) FlushTask(taskID string) {
	var ready []bufferedTaskDelta
	b.mu.Lock()
	for key, entry := range b.entries {
		if entry.taskID != taskID {
			continue
		}
		delete(b.entries, key)
		entry.timer.Stop()
		ready = append(ready, *entry)
	}
	b.mu.Unlock()
	for _, entry := range ready {
		b.flush(entry.taskID, entry.runtime, entry.kind, entry.payload)
	}
}

func (b *TaskDeltaBuffer) flushKey(key string) {
	b.mu.Lock()
	entry := b.entries[key]
	if entry != nil {
		delete(b.entries, key)
	}
	b.mu.Unlock()
	if entry != nil {
		b.flush(entry.taskID, entry.runtime, entry.kind, entry.payload)
	}
}
