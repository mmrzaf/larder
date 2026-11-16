package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrJobConflict means a job of the same kind is already running.
	ErrJobConflict = errors.New("job already running")
	// ErrJobNotFound means the job id is unknown or expired.
	ErrJobNotFound = errors.New("job not found")
)

// JobProgress is the observable progress of a long operation.
type JobProgress struct {
	Items   int64  `json:"items"`
	Bytes   int64  `json:"bytes"`
	Message string `json:"message,omitempty"`
}

// JobView is the JSON representation of a job.
type JobView struct {
	ID       string      `json:"id"`
	Kind     string      `json:"kind"`
	State    string      `json:"state"`
	Progress JobProgress `json:"progress"`
	Result   any         `json:"result,omitempty"`
	Error    string      `json:"error,omitempty"`
	Started  time.Time   `json:"started"`
	Finished time.Time   `json:"finished,omitempty"`
}

// Job is one long-running admin operation.
type Job struct {
	ID    string
	Kind  string
	State string
	group string

	mu       sync.Mutex
	progress JobProgress
	result   any
	err      string
	started  time.Time
	finished time.Time
	cancel   context.CancelFunc
	done     chan struct{}
}

// AddProgress increments the job counters.
func (j *Job) AddProgress(items, bytes int64) {
	j.mu.Lock()
	j.progress.Items += items
	j.progress.Bytes += bytes
	j.mu.Unlock()
}

// SetProgress replaces the job counters and message.
func (j *Job) SetProgress(progress JobProgress) {
	j.mu.Lock()
	j.progress = progress
	j.mu.Unlock()
}

// SetResult records a result before the job completes.
func (j *Job) SetResult(result any) {
	j.mu.Lock()
	j.result = result
	j.mu.Unlock()
}

// Done is closed when the job finishes.
func (j *Job) Done() <-chan struct{} { return j.done }

func (j *Job) view() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return JobView{
		ID: j.ID, Kind: j.Kind, State: j.State, Progress: j.progress,
		Result: j.result, Error: j.err, Started: j.started, Finished: j.finished,
	}
}

// JobManager runs one job per kind and retains completed jobs.
type JobManager struct {
	mu        sync.Mutex
	clock     interface{ Now() time.Time }
	retention time.Duration
	jobs      map[string]*Job
	running   map[string]string
	groups    map[string]string
	closed    bool
	wg        sync.WaitGroup
}

// NewJobManager builds a manager retaining completed jobs for retention.
func NewJobManager(clock interface{ Now() time.Time }, retention time.Duration) *JobManager {
	if clock == nil {
		clock = realClock{}
	}
	if retention <= 0 {
		retention = time.Hour
	}
	return &JobManager{
		clock: clock, retention: retention,
		jobs: make(map[string]*Job), running: make(map[string]string),
		groups: make(map[string]string),
	}
}

// Start launches a job, rejecting a second running job of the same kind.
func (m *JobManager) Start(kind string, fn func(context.Context, *Job) error) (*Job, error) {
	return m.start(kind, "", fn)
}

// StartExclusive launches a job that shares an exclusivity group with other
// kinds, such as storage maintenance.
func (m *JobManager) StartExclusive(kind, group string, fn func(context.Context, *Job) error) (*Job, error) {
	if group == "" {
		return nil, fmt.Errorf("job: group is required")
	}
	return m.start(kind, group, fn)
}

// RunningID returns the id of the running job in a group, or an empty string.
func (m *JobManager) RunningID(group string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.groups[group]
}

func (m *JobManager) start(kind, group string, fn func(context.Context, *Job) error) (*Job, error) {
	if kind == "" || fn == nil {
		return nil, fmt.Errorf("job: kind and function are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	if m.closed {
		return nil, fmt.Errorf("job manager is closed")
	}
	if _, exists := m.running[kind]; exists {
		return nil, ErrJobConflict
	}
	if group != "" {
		if _, exists := m.groups[group]; exists {
			return nil, ErrJobConflict
		}
	}
	job := &Job{
		ID: newJobID(), Kind: kind, State: "running", group: group,
		started: m.clock.Now(), done: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	m.jobs[job.ID] = job
	m.running[kind] = job.ID
	if group != "" {
		m.groups[group] = job.ID
	}
	m.wg.Add(1)
	go m.run(ctx, job, fn)
	return job, nil
}

func (m *JobManager) run(ctx context.Context, job *Job, fn func(context.Context, *Job) error) {
	defer m.wg.Done()
	err := fn(ctx, job)
	job.mu.Lock()
	job.finished = m.clock.Now()
	if err != nil {
		job.State = "failed"
		job.err = err.Error()
	} else {
		job.State = "done"
	}
	job.cancel()
	job.mu.Unlock()
	close(job.done)

	m.mu.Lock()
	delete(m.running, job.Kind)
	if job.group != "" && m.groups[job.group] == job.ID {
		delete(m.groups, job.group)
	}
	m.cleanupLocked()
	m.mu.Unlock()
}

// Get returns a job view, or ErrJobNotFound after retention.
func (m *JobManager) Get(id string) (JobView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	job := m.jobs[id]
	if job == nil {
		return JobView{}, ErrJobNotFound
	}
	return job.view(), nil
}

func (m *JobManager) cleanupLocked() {
	if m.retention <= 0 {
		return
	}
	now := m.clock.Now()
	for id, job := range m.jobs {
		job.mu.Lock()
		finished := job.finished
		state := job.State
		job.mu.Unlock()
		if state != "running" && !finished.IsZero() && now.Sub(finished) >= m.retention {
			delete(m.jobs, id)
		}
	}
}

// Close cancels running jobs and waits for them to stop.
func (m *JobManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	jobs := make([]*Job, 0, len(m.running))
	for _, id := range m.running {
		if job := m.jobs[id]; job != nil {
			jobs = append(jobs, job)
		}
	}
	m.mu.Unlock()
	for _, job := range jobs {
		if job.cancel != nil {
			job.cancel()
		}
	}
	m.wg.Wait()
	return nil
}

func newJobID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
