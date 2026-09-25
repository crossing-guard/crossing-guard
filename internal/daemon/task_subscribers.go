package daemon

import "sync"

const taskSubscriberQueue = 128

type taskSubscription struct {
	ID     uint64
	Events <-chan TaskEvent
}

type taskSubscriber struct {
	taskID string
	ch     chan TaskEvent
}

// TaskSubscriberHub fans out events that have already been accepted by the task
// repository. A slow browser is disconnected; it can replay from its last contiguous
// cursor and can never backpressure a vendor process reader.
type TaskSubscriberHub struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]taskSubscriber
}

func NewTaskSubscriberHub() *TaskSubscriberHub {
	return &TaskSubscriberHub{subscribers: map[uint64]taskSubscriber{}}
}

func (h *TaskSubscriberHub) Subscribe(taskID string) taskSubscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	ch := make(chan TaskEvent, taskSubscriberQueue)
	h.subscribers[h.nextID] = taskSubscriber{taskID: taskID, ch: ch}
	return taskSubscription{ID: h.nextID, Events: ch}
}

func (h *TaskSubscriberHub) Unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sub, ok := h.subscribers[id]; ok {
		delete(h.subscribers, id)
		close(sub.ch)
	}
}

func (h *TaskSubscriberHub) Publish(event TaskEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, sub := range h.subscribers {
		if sub.taskID != "" && sub.taskID != event.TaskID {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			delete(h.subscribers, id)
			close(sub.ch)
		}
	}
}
