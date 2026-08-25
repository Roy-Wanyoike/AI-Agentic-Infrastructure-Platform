package queue

import (
	"sync"
	"time"
)

type Task struct {
	ID        string
	Type      string
	Payload   map[string]any
	Status    string
	Attempts  int
	CreatedAt time.Time
	UpdatedAt time.Time
	LastError string
}

type Queue struct {
	mu    sync.Mutex
	tasks []*Task
}

func NewQueue() *Queue {
	return &Queue{tasks: make([]*Task, 0)}
}

func (q *Queue) Enqueue(taskType string, payload map[string]any) *Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	task := &Task{
		ID:        taskType + "-" + time.Now().UTC().Format(time.RFC3339Nano),
		Type:      taskType,
		Payload:   payload,
		Status:    "queued",
		Attempts:  0,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	q.tasks = append(q.tasks, task)
	return task
}

func (q *Queue) MarkStarted(task *Task) {
	if task == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	task.Attempts++
	task.Status = "running"
	task.UpdatedAt = time.Now().UTC()
}

func (q *Queue) MarkFailed(task *Task, errMsg string) {
	if task == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	task.LastError = errMsg
	task.UpdatedAt = time.Now().UTC()

	if task.Attempts >= 4 {
		task.Status = "dead_letter"
		return
	}

	task.Status = "queued"
}

func (q *Queue) Length() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

func (q *Queue) Peek() *Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return nil
	}
	return q.tasks[0]
}

func (q *Queue) Ack(task *Task) {
	if task == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, queued := range q.tasks {
		if queued.ID == task.ID {
			q.tasks = append(q.tasks[:i], q.tasks[i+1:]...)
			return
		}
	}
}
func (q *Queue) Dequeue() *Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return nil
	}
	task := q.tasks[0]
	q.tasks = q.tasks[1:]
	return task
}

func (q *Queue) Requeue(task *Task) {
	if task == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	task.Status = "queued"
	task.UpdatedAt = time.Now().UTC()
	q.tasks = append(q.tasks, task)
}

type Worker struct {
	q      *Queue
	handle func(*Task) error
}

func NewWorker(q *Queue, handle func(*Task) error) *Worker {
	return &Worker{q: q, handle: handle}
}

func (w *Worker) ProcessNext() error {
	if w == nil || w.q == nil {
		return nil
	}
	task := w.q.Dequeue()
	if task == nil {
		return nil
	}
	w.q.MarkStarted(task)
	if w.handle != nil {
		if err := w.handle(task); err != nil {
			w.q.MarkFailed(task, err.Error())
			if task.Status == "dead_letter" {
				return nil
			}
			w.q.Requeue(task)
			return err
		}
	}
	w.q.Ack(task)
	return nil
}
