package daemon

import "fmt"

type TaskLifecycle string

const (
	TaskQueued      TaskLifecycle = "queued"
	TaskStarting    TaskLifecycle = "starting"
	TaskRunning     TaskLifecycle = "running"
	TaskCompleted   TaskLifecycle = "completed"
	TaskInterrupted TaskLifecycle = "interrupted"
	TaskFailed      TaskLifecycle = "failed"
	TaskUnknown     TaskLifecycle = "unknown"
)

type TaskStateMachine struct{}

func (TaskStateMachine) Validate(from, to TaskLifecycle) error {
	allowed := map[TaskLifecycle]map[TaskLifecycle]bool{
		TaskQueued:   {TaskStarting: true, TaskInterrupted: true, TaskFailed: true, TaskUnknown: true},
		TaskStarting: {TaskRunning: true, TaskInterrupted: true, TaskFailed: true, TaskUnknown: true},
		TaskRunning:  {TaskCompleted: true, TaskInterrupted: true, TaskFailed: true, TaskUnknown: true},
	}
	if !allowed[from][to] {
		return fmt.Errorf("runtime task transition %s -> %s is not allowed", from, to)
	}
	return nil
}

func terminalTaskLifecycle(state TaskLifecycle) bool {
	switch state {
	case TaskCompleted, TaskInterrupted, TaskFailed, TaskUnknown:
		return true
	default:
		return false
	}
}
