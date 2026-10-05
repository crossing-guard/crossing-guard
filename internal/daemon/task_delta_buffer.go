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
// window with synchronous ordered flushes. Flush callbacks must not reenter the
// buffer; FlushTask waits for timer publication before the next event boundary.
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
	defer b.mu.Unlock()
	entry := b.entries[key]
	if entry != nil && len(anyString(entry.payload["text"]))+len(text) > taskDeltaFlushBytes {
		delete(b.entries, key)
		entry.timer.Stop()
		b.flush(entry.taskID, entry.runtime, entry.kind, entry.payload)
		entry = nil
	}
	if entry == nil {
		copied := ChatEvent{}
		for name, value := range payload {
			copied[name] = value
		}
		entry = &bufferedTaskDelta{taskID: taskID, runtime: runtime, kind: kind, payload: copied}
		entry.timer = time.AfterFunc(taskDeltaFlushInterval, func() { b.flushKey(key, entry) })
		b.entries[key] = entry
	} else {
		entry.payload["text"] = anyString(entry.payload["text"]) + text
	}
}

func (b *TaskDeltaBuffer) FlushTask(taskID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, entry := range b.entries {
		if entry.taskID != taskID {
			continue
		}
		delete(b.entries, key)
		entry.timer.Stop()
		b.flush(entry.taskID, entry.runtime, entry.kind, entry.payload)
	}
}

func (b *TaskDeltaBuffer) flushKey(key string, expected *bufferedTaskDelta) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entries[key]
	if entry == expected {
		delete(b.entries, key)
		b.flush(entry.taskID, entry.runtime, entry.kind, entry.payload)
	}
}
