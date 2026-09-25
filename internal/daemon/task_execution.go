package daemon

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
)

const (
	taskMaxRunning = 3
	taskMaxRuntime = 2
	taskMaxQueued  = 16
)

var ErrTaskAdmissionBusy = errors.New("runtime task capacity is full")

type executionReservation struct {
	runtime string
	active  bool
	used    bool
}

type taskExecutionLaunch struct {
	taskID  string
	runtime string
	cmd     *exec.Cmd
	driver  ChatDriver
	started func()
	event   func(ChatEvent)
	done    func(executionOutcome)
	// admit is re-evaluated at launch, when a queued reservation is finally
	// promoted: a session quiet at request time may be in use by then. A nil
	// admit means nothing to re-check.
	admit func() error
}

type executionOutcome struct {
	Err         error
	Interrupted bool
}

type activeTaskExecution struct {
	launch      taskExecutionLaunch
	interrupted bool
}

// TaskExecutionRegistry is the sole owner of live process handles, admission slots,
// and queued launch objects. Stored PIDs never enter this type and cannot restore
// control authority after daemon restart.
type TaskExecutionRegistry struct {
	mu          sync.Mutex
	running     int
	perRuntime  map[string]int
	queuedCount int
	active      map[string]*activeTaskExecution
	queue       []taskExecutionLaunch
}

func NewTaskExecutionRegistry() *TaskExecutionRegistry {
	return &TaskExecutionRegistry{perRuntime: map[string]int{}, active: map[string]*activeTaskExecution{}}
}

func (r *TaskExecutionRegistry) Reserve(runtime string) (*executionReservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reservation := &executionReservation{runtime: runtime}
	if r.running < taskMaxRunning && r.perRuntime[runtime] < taskMaxRuntime {
		reservation.active = true
		r.running++
		r.perRuntime[runtime]++
		return reservation, nil
	}
	if r.queuedCount >= taskMaxQueued {
		return nil, ErrTaskAdmissionBusy
	}
	r.queuedCount++
	return reservation, nil
}

func (r *TaskExecutionRegistry) Release(reservation *executionReservation) {
	if reservation == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if reservation.used {
		return
	}
	reservation.used = true
	if reservation.active {
		r.running--
		r.perRuntime[reservation.runtime]--
	} else {
		r.queuedCount--
	}
}

func (r *TaskExecutionRegistry) Schedule(reservation *executionReservation, launch taskExecutionLaunch) {
	r.mu.Lock()
	if reservation == nil || reservation.used {
		r.mu.Unlock()
		launch.done(executionOutcome{Err: errors.New("invalid task execution reservation")})
		return
	}
	reservation.used = true
	if !reservation.active {
		r.queue = append(r.queue, launch)
		r.mu.Unlock()
		return
	}
	r.active[launch.taskID] = &activeTaskExecution{launch: launch}
	r.mu.Unlock()
	go r.run(launch)
}

func (r *TaskExecutionRegistry) run(launch taskExecutionLaunch) {
	if launch.admit != nil {
		if err := launch.admit(); err != nil {
			r.finish(launch, executionOutcome{Err: err})
			return
		}
	}
	outcome := runTaskProcess(launch, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.active[launch.taskID] != nil && r.active[launch.taskID].interrupted
	})
	r.finish(launch, outcome)
}

func (r *TaskExecutionRegistry) finish(launch taskExecutionLaunch, outcome executionOutcome) {
	r.mu.Lock()
	delete(r.active, launch.taskID)
	r.running--
	r.perRuntime[launch.runtime]--
	next := r.promoteLocked()
	r.mu.Unlock()
	launch.done(outcome)
	if next != nil {
		go r.run(*next)
	}
}

func (r *TaskExecutionRegistry) promoteLocked() *taskExecutionLaunch {
	for index, launch := range r.queue {
		if r.running >= taskMaxRunning || r.perRuntime[launch.runtime] >= taskMaxRuntime {
			continue
		}
		r.queue = append(r.queue[:index], r.queue[index+1:]...)
		r.queuedCount--
		r.running++
		r.perRuntime[launch.runtime]++
		r.active[launch.taskID] = &activeTaskExecution{launch: launch}
		return &launch
	}
	return nil
}

func (r *TaskExecutionRegistry) Interrupt(taskID string) (bool, error) {
	r.mu.Lock()
	if active := r.active[taskID]; active != nil {
		active.interrupted = true
		cmd := active.launch.cmd
		r.mu.Unlock()
		if err := interruptTaskProcess(cmd); err != nil {
			return false, fmt.Errorf("interrupt runtime task: %w", err)
		}
		return true, nil
	}
	for index, launch := range r.queue {
		if launch.taskID != taskID {
			continue
		}
		r.queue = append(r.queue[:index], r.queue[index+1:]...)
		r.queuedCount--
		r.mu.Unlock()
		launch.done(executionOutcome{Interrupted: true})
		return true, nil
	}
	r.mu.Unlock()
	return false, nil
}
