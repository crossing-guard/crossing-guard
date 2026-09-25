package daemon

import (
	"encoding/json"
	"errors"
)

func (tasks *TaskApplicationService) taskEventsThrough(taskID string, sequence int64) ([]TaskEvent, error) {
	task, found, err := tasks.Task(taskID)
	if err != nil {
		return nil, err
	}
	if !found || sequence <= 0 || sequence > task.LastSequence {
		return nil, errors.New("observed task event context is unavailable at the requested cutoff")
	}
	after := sequence - 512
	if after < 0 {
		after = 0
	}
	events, err := tasks.Events(taskID, after, 512)
	if err != nil {
		return nil, err
	}
	end := 0
	for end < len(events) && events[end].Sequence <= sequence {
		end++
	}
	return events[:end], nil
}

func (tasks *TaskApplicationService) taskMessageThrough(taskID string, sequence int64) (string, error) {
	events, err := tasks.taskEventsThrough(taskID, sequence)
	if err != nil {
		return "", err
	}
	final := ""
	for _, event := range events {
		if event.Kind == "message.completed" {
			if text, _ := event.Payload["text"].(string); text != "" {
				final = text
			}
		}
	}
	return truncate(final, 262144), nil
}

// taskMessagesContext projects the existing observed stream. Missing user input,
// an old event window and discarded large entries are disclosed, never invented.
func (tasks *TaskApplicationService) taskMessagesContext(taskID string, sequence int64, maxBytes int) (string, error) {
	events, err := tasks.taskEventsThrough(taskID, sequence)
	if err != nil {
		return "", err
	}
	rows := []map[string]any{}
	omitted := int64(0)
	if sequence > 512 {
		omitted = sequence - 512
	}
	for _, event := range events {
		switch event.Kind {
		case "message.completed", "tool.started", "tool.completed":
			text, _ := event.Payload["text"].(string)
			name, _ := event.Payload["name"].(string)
			rows = append(rows, map[string]any{"sequence": event.Sequence, "kind": event.Kind, "text": text, "name": name})
		}
	}
	for {
		body, err := json.Marshal(map[string]any{"coverage": "observed assistant/tool events only; original user input is not in this stream", "through_sequence": sequence, "omitted_events": omitted, "events": rows})
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(string(body))
		if err != nil {
			return "", err
		}
		if len(encoded) <= maxBytes {
			return string(body), nil
		}
		if len(rows) == 0 {
			return "", errors.New("task.messages byte limit cannot contain coverage metadata")
		}
		rows = rows[1:]
		omitted++
	}
}
