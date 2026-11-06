package browser

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

type UserError struct{ Message string }

func (e *UserError) Error() string { return e.Message }

type LoginRunner interface {
	Login(context.Context, func(Update)) (*Result, error)
}

var ErrBusy = errors.New("Сейчас много входов одновременно, попробуйте через минуту.")
var ErrMissing = errors.New("Вход не найден или истёк")
var ErrIncomplete = errors.New("Вход ещё не завершён")

type JobState struct {
	ID, Status string
	Label      any
	AccountID  string
	Created    time.Time
	Error, QR  *string
}
type job struct {
	state   JobState
	digest  [32]byte
	created time.Time
	result  *Result
	claimed bool
	cancel  context.CancelFunc
}
type Jobs struct {
	mu        sync.Mutex
	entries   map[string]*job
	runner    LoginRunner
	root      context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closed    bool
	TTL       time.Duration
	MaxActive int
	Retain    time.Duration
	Now       func() time.Time
}

func NewJobs(ctx context.Context, runner LoginRunner) *Jobs {
	root, cancel := context.WithCancel(ctx)
	jobs := &Jobs{entries: map[string]*job{}, runner: runner, root: root, cancel: cancel, TTL: 600 * time.Second, MaxActive: 16, Now: time.Now}
	jobs.wg.Add(1)
	go func() {
		defer jobs.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-root.Done():
				return
			case <-ticker.C:
				jobs.Prune()
			}
		}
	}()
	return jobs
}
func token(size int) (string, error) {
	data := make([]byte, size)
	_, err := rand.Read(data)
	return base64.RawURLEncoding.EncodeToString(data), err
}
func (j *Jobs) pruneLocked() {
	for id, entry := range j.entries {
		retain := j.TTL + 300*time.Second
		if j.Retain > retain {
			retain = j.Retain
		}
		if j.Now().Sub(entry.created) > retain {
			entry.cancel()
			entry.result = nil
			delete(j.entries, id)
		}
	}
}
func (j *Jobs) Prune() { j.mu.Lock(); defer j.mu.Unlock(); j.pruneLocked() }
func (j *Jobs) Start() (JobState, string, error) {
	return j.StartWith(j.runner, "", "Не удалось войти через МИРЭА. Попробуйте ещё раз.")
}
func (j *Jobs) StartWith(runner LoginRunner, label any, failureMessage string) (JobState, string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return JobState{}, "", ErrClosed
	}
	j.pruneLocked()
	active := 0
	for _, entry := range j.entries {
		switch entry.state.Status {
		case "waiting", "login", "finishing":
			active++
		}
	}
	if active >= j.MaxActive {
		return JobState{}, "", ErrBusy
	}
	id, err := token(18)
	if err != nil {
		return JobState{}, "", err
	}
	capability, err := token(32)
	if err != nil {
		return JobState{}, "", err
	}
	ctx, cancel := context.WithTimeout(j.root, j.TTL)
	entry := &job{state: JobState{ID: id, Status: "waiting", Label: label, Created: j.Now()}, digest: sha256.Sum256([]byte(capability)), created: j.Now(), cancel: cancel}
	j.entries[id] = entry
	j.wg.Add(1)
	go j.run(ctx, entry, runner, failureMessage)
	return entry.state, capability, nil
}
func (j *Jobs) run(ctx context.Context, entry *job, runner LoginRunner, failureMessage string) {
	defer j.wg.Done()
	defer entry.cancel()
	result, err := runner.Login(ctx, func(update Update) {
		j.mu.Lock()
		defer j.mu.Unlock()
		entry.state.Status = update.Status
		entry.state.QR = nil
		if update.QR != "" {
			value := update.QR
			entry.state.QR = &value
		}
	})
	j.mu.Lock()
	defer j.mu.Unlock()
	entry.state.QR = nil
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, ErrExpired) {
		entry.state.Status = "expired"
		return
	}
	if ctx.Err() != nil {
		entry.state.Status = "cancelled"
		return
	}
	if err != nil || result == nil {
		entry.state.Status = "failed"
		message := failureMessage
		var public *UserError
		if errors.As(err, &public) {
			message = public.Message
		}
		entry.state.Error = &message
		return
	}
	entry.result = result
	entry.state.AccountID = result.AccountID
	entry.state.Status = "complete"
}

// AdminState and Cancel are for an authenticated administrative controller only.
func (j *Jobs) AdminState(id string) (JobState, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry := j.entries[id]
	if entry == nil {
		return JobState{}, ErrMissing
	}
	return entry.state, nil
}
func (j *Jobs) Cancel(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry := j.entries[id]
	if entry == nil {
		return ErrMissing
	}
	entry.cancel()
	return nil
}
func (j *Jobs) find(id, capability string) (*job, error) {
	entry := j.entries[id]
	digest := sha256.Sum256([]byte(capability))
	if entry == nil || capability == "" || subtle.ConstantTimeCompare(digest[:], entry.digest[:]) != 1 {
		return nil, ErrMissing
	}
	return entry, nil
}
func (j *Jobs) State(id, capability string) (JobState, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, err := j.find(id, capability)
	if err != nil {
		return JobState{}, err
	}
	return entry.state, nil
}

// Claim removes credentials while holding the same lock as status updates.
func (j *Jobs) Claim(id, capability string) (*Result, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, err := j.find(id, capability)
	if err != nil {
		return nil, err
	}
	if entry.state.Status != "complete" || entry.result == nil || entry.claimed {
		return nil, ErrIncomplete
	}
	entry.claimed = true
	result := entry.result
	entry.result = nil
	return result, nil
}
func (j *Jobs) Close() {
	j.mu.Lock()
	j.closed = true
	j.cancel()
	j.mu.Unlock()
	j.wg.Wait()
	j.mu.Lock()
	clear(j.entries)
	j.mu.Unlock()
}
