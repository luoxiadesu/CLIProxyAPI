package tasks

import (
	"container/heap"
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultWorkers is the fallback number of concurrent accounts.
	DefaultWorkers = 4
	// maxWorkers caps the worker pool even for programmatic configurations.
	maxWorkers = 64
	// defaultTaskTimeout bounds a task run when neither the spec nor the
	// configuration provides a timeout.
	defaultTaskTimeout = 30 * time.Second
	// maxTimerWait caps a single timer sleep. CLOCK_MONOTONIC stops during
	// suspend, so capping the wait makes the loop notice elapsed wall time
	// promptly after resume.
	maxTimerWait = 30 * time.Second
	// maxBackoff caps the failure retry delay.
	maxBackoff = 6 * time.Hour
	// orphanStateRetention keeps state of removed accounts for a while before
	// deleting it.
	orphanStateRetention = 7 * 24 * time.Hour
	// providerWorkBuddy is the only provider the scheduler schedules for.
	providerWorkBuddy = "workbuddy"
)

// backoffSteps is the retry delay sequence after consecutive failures. Beyond
// the sequence the delay doubles until it reaches maxBackoff.
var backoffSteps = []time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
}

// Scheduler runs injected tasks for matching auth records.
//
// Scheduling is per auth: one heap entry tracks the earliest due task of an
// account, and a per-account mutex guarantees that tasks of the same account
// never overlap while different accounts may run concurrently.
type Scheduler struct {
	manager *coreauth.Manager
	store   StateStore

	mu       sync.Mutex
	cfg      *internalconfig.Config
	tasks    []Task
	loc      *time.Location
	timeout  time.Duration
	workers  int
	stateDir string

	generation uint64
	run        *schedulerRun

	queue     taskHeap
	index     map[string]*taskHeapItem
	states    map[string]State
	authLocks map[string]*authLock

	persistMu sync.Mutex
	wakeCh    chan struct{}
	// scheduleCh asks the loop to re-arm the timer after a worker reschedules
	// an account without a full rebuild.
	scheduleCh chan struct{}

	// jitter and now are overridable in tests.
	jitter func(time.Duration) time.Duration
	now    func() time.Time
}

// schedulerRun holds the state of one Start/Stop cycle.
type schedulerRun struct {
	cancel context.CancelFunc
	jobs   chan taskJob
	wg     sync.WaitGroup
}

// taskJob asks a worker to evaluate and run the due tasks of one account.
type taskJob struct {
	authID     string
	generation uint64
}

// NewScheduler creates a scheduler. cfg may be replaced later with ApplyConfig;
// tasks may be replaced later with SetTasks.
func NewScheduler(cfg *internalconfig.Config, manager *coreauth.Manager, store StateStore, taskList []Task) *Scheduler {
	if store == nil {
		store = nopStateStore{}
	}
	s := &Scheduler{
		manager:    manager,
		store:      store,
		cfg:        cfg,
		loc:        resolveLocation(cfg),
		timeout:    resolveTimeout(cfg),
		workers:    resolveWorkers(cfg),
		queue:      nil,
		index:      make(map[string]*taskHeapItem),
		states:     make(map[string]State),
		authLocks:  make(map[string]*authLock),
		wakeCh:     make(chan struct{}, 1),
		scheduleCh: make(chan struct{}, 1),
		jitter:     jitterDuration,
		now:        time.Now,
	}
	if cfg != nil {
		if dir, err := ResolveStateDir(cfg); err != nil {
			log.Warnf("workbuddy tasks: resolve state dir: %v", err)
		} else {
			s.stateDir = dir
		}
	}
	s.tasks = append([]Task(nil), taskList...)
	return s
}

// Start launches the scheduling loop and workers. It is a no-op when the
// scheduler is already running or has no auth manager.
func (s *Scheduler) Start(ctx context.Context) {
	if s == nil || s.manager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.run != nil {
		s.mu.Unlock()
		return
	}
	workers := s.workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	buffer := workers * 2
	if buffer < 8 {
		buffer = 8
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &schedulerRun{
		cancel: cancel,
		jobs:   make(chan taskJob, buffer),
	}
	// Register the wait before publishing the run so a concurrent Stop always
	// waits for the loop goroutine.
	run.wg.Add(1)
	s.run = run
	s.mu.Unlock()

	go s.runLoop(runCtx, run, workers)
}

// Stop cancels the loop and waits for workers to exit. It is idempotent.
func (s *Scheduler) Stop() {
	_ = s.StopContext(context.Background())
}

// StopContext cancels the loop and waits for workers to exit or for ctx to
// expire. It is idempotent.
func (s *Scheduler) StopContext(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	run := s.run
	s.run = nil
	s.mu.Unlock()
	if run == nil {
		return nil
	}
	run.cancel()
	done := make(chan struct{})
	go func() {
		run.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ApplyConfig swaps the runtime configuration and rebuilds the schedule.
// Changes that cannot be applied without a restart (state directory, worker
// count) are logged as warnings.
func (s *Scheduler) ApplyConfig(cfg *internalconfig.Config) {
	if s == nil || cfg == nil {
		return
	}
	s.mu.Lock()
	s.cfg = cfg
	s.timeout = resolveTimeout(cfg)
	s.loc = resolveLocation(cfg)
	if dir, err := ResolveStateDir(cfg); err != nil {
		log.Warnf("workbuddy tasks: resolve state dir: %v", err)
	} else if s.stateDir != "" && dir != s.stateDir {
		log.Warnf("workbuddy tasks: state-dir changed from %q to %q; restart required to apply", s.stateDir, dir)
	}
	workers := resolveWorkers(cfg)
	if s.run != nil {
		if workers != s.workers {
			log.Warnf("workbuddy tasks: workers changed from %d to %d; restart required to apply", s.workers, workers)
		}
	} else {
		s.workers = workers
	}
	s.generation++
	s.mu.Unlock()
	s.Wake()
}

// SetTasks replaces the scheduled tasks and rebuilds the schedule.
func (s *Scheduler) SetTasks(taskList []Task) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.tasks = append([]Task(nil), taskList...)
	s.generation++
	s.mu.Unlock()
	s.Wake()
}

// Wake requests an asynchronous schedule rebuild.
func (s *Scheduler) Wake() {
	if s == nil || s.wakeCh == nil {
		return
	}
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// wakeTimer requests a timer re-arm without a full rebuild.
func (s *Scheduler) wakeTimer() {
	if s == nil || s.scheduleCh == nil {
		return
	}
	select {
	case s.scheduleCh <- struct{}{}:
	default:
	}
}

// runLoop owns the workers and the scheduling loop for one Start/Stop cycle.
func (s *Scheduler) runLoop(ctx context.Context, run *schedulerRun, workers int) {
	defer run.wg.Done()

	var workerWG sync.WaitGroup
	workerWG.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer workerWG.Done()
			s.worker(ctx, run)
		}()
	}

	s.rebuild(ctx)
	s.loop(ctx, run)

	close(run.jobs)
	workerWG.Wait()
}

// worker consumes due-account jobs until the run context is canceled.
func (s *Scheduler) worker(ctx context.Context, run *schedulerRun) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-run.jobs:
			if !ok {
				return
			}
			s.execute(ctx, job)
		}
	}
}

// loop dispatches due accounts and rebuilds on wake.
func (s *Scheduler) loop(ctx context.Context, run *schedulerRun) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()

	var timerCh <-chan time.Time
	s.resetTimer(timer, &timerCh)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wakeCh:
			s.rebuild(ctx)
			s.resetTimer(timer, &timerCh)
		case <-s.scheduleCh:
			s.resetTimer(timer, &timerCh)
		case <-timerCh:
			s.dispatchDue(ctx, run)
			s.resetTimer(timer, &timerCh)
		}
	}
}

// dispatchDue pops every account whose earliest task is due and queues a job.
func (s *Scheduler) dispatchDue(ctx context.Context, run *schedulerRun) {
	due, generation := s.popDue(s.now())
	for _, authID := range due {
		job := taskJob{authID: authID, generation: generation}
		select {
		case <-ctx.Done():
			return
		case run.jobs <- job:
		}
	}
}

// execute runs every due task of one account. It holds the per-account lock so
// tasks of the same account are serialized.
func (s *Scheduler) execute(ctx context.Context, job taskJob) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	if job.generation != s.generation {
		s.mu.Unlock()
		return
	}
	taskList := append([]Task(nil), s.tasks...)
	loc := s.loc
	timeout := s.timeout
	lock := s.authLockLocked(job.authID)
	s.mu.Unlock()
	if lock == nil {
		return
	}

	if !lock.acquire(ctx) {
		return
	}
	defer lock.release()

	// Re-check the generation after waiting: a config change may have replaced
	// the schedule (and the per-account lock) while this job was queued.
	s.mu.Lock()
	if job.generation != s.generation {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	auth, ok := s.manager.GetByID(job.authID)
	if !ok || auth == nil || auth.Disabled || !strings.EqualFold(auth.Provider, providerWorkBuddy) {
		s.removeAuth(job.authID)
		return
	}

	s.mu.Lock()
	state := cloneState(s.states[job.authID])
	s.mu.Unlock()
	if state.Version == 0 {
		state.Version = CurrentStateVersion
	}
	if state.AuthID == "" {
		state.AuthID = auth.ID
	}
	if state.Provider == "" {
		state.Provider = auth.Provider
	}
	if state.UID == "" {
		state.UID = authUID(auth)
	}
	if state.Tasks == nil {
		state.Tasks = make(map[string]TaskState)
	}

	now := s.now()
	ran := false
	for _, task := range taskList {
		spec := task.Spec()
		if !spec.Enabled {
			continue
		}
		if spec.Provider != "" && !strings.EqualFold(spec.Provider, auth.Provider) {
			continue
		}
		taskState := state.Tasks[spec.Name]
		dueAt, due := nextTaskRun(now, spec, loc, taskState)
		if !due || dueAt.After(now) {
			continue
		}
		ran = true
		runCtx := ctx
		taskTimeout := timeout
		if spec.Timeout > 0 {
			taskTimeout = spec.Timeout
		}
		var cancel context.CancelFunc
		if taskTimeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, taskTimeout)
		}
		runCtx = WithTaskState(runCtx, &taskState)
		errRun := runTask(task, runCtx, auth)
		if cancel != nil {
			cancel()
		}

		finishedAt := s.now()
		taskState.LastRun = finishedAt
		if errRun != nil {
			taskState.ConsecutiveFailures++
			taskState.LastStatus = StatusError
			taskState.LastError = errRun.Error()
			taskState.NextRun = finishedAt.Add(s.backoff(taskState.ConsecutiveFailures))
			log.WithFields(log.Fields{
				"auth_id":     auth.ID,
				"task":        spec.Name,
				"failures":    taskState.ConsecutiveFailures,
				"retry_after": taskState.NextRun.Sub(finishedAt).String(),
			}).Warnf("workbuddy task failed: %v", errRun)
		} else {
			taskState.ConsecutiveFailures = 0
			taskState.LastStatus = StatusOK
			taskState.LastError = ""
			taskState.NextRun, _ = NextDue(finishedAt, spec, loc, finishedAt)
			if spec.DailyAt != "" {
				taskState.LastWindow = dueAt.In(loc).Format("2006-01-02")
			}
			log.WithFields(log.Fields{
				"auth_id": auth.ID,
				"task":    spec.Name,
			}).Debug("workbuddy task completed")
		}
		state.Tasks[spec.Name] = taskState
	}

	s.mu.Lock()
	state.UpdatedAt = s.now()
	if existing, ok := s.states[job.authID]; ok {
		// Merge even when the generation changed: rebuild may have loaded an
		// older on-disk state and would otherwise schedule a daily task twice.
		s.states[job.authID] = mergeState(existing, state)
	} else {
		s.states[job.authID] = state
	}
	s.mu.Unlock()

	// Persist even when a config change raced with the run: the task did run,
	// and losing its state could execute a daily task twice.
	if ran {
		s.persist(state)
	}
	s.scheduleAuth(job.authID, job.generation)
	s.wakeTimer()
}

// runTask isolates a task panic so one bad task cannot kill a worker.
func runTask(task Task, ctx context.Context, auth *coreauth.Auth) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("task panic: %v", recovered)
		}
	}()
	return task.Run(ctx, auth)
}

// scheduleAuth recomputes and stores the next run for one account.
func (s *Scheduler) scheduleAuth(authID string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return
	}
	state, ok := s.states[authID]
	if !ok {
		return
	}
	next, ok := earliestNext(s.now(), s.tasks, s.loc, state)
	if !ok {
		s.removeLocked(authID)
		return
	}
	s.upsertLocked(authID, next)
}

// rebuild reloads auth records and their persisted state, then replaces the
// heap. Disk I/O happens outside the scheduler lock.
func (s *Scheduler) rebuild(ctx context.Context) {
	s.mu.Lock()
	generation := s.generation
	enabled := s.cfg != nil && s.cfg.WorkBuddy.Enabled
	taskList := append([]Task(nil), s.tasks...)
	loc := s.loc
	s.mu.Unlock()

	if !enabled {
		s.mu.Lock()
		if generation == s.generation {
			s.queue = nil
			s.index = make(map[string]*taskHeapItem)
			s.states = make(map[string]State)
		}
		s.mu.Unlock()
		return
	}

	auths := s.manager.List()
	wanted := make([]*coreauth.Auth, 0, len(auths))
	wantedIDs := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if auth == nil || auth.Disabled {
			continue
		}
		if !strings.EqualFold(auth.Provider, providerWorkBuddy) {
			continue
		}
		wanted = append(wanted, auth)
		wantedIDs[auth.ID] = struct{}{}
	}

	loaded := make(map[string]State, len(wanted))
	for _, auth := range wanted {
		state, errLoad := s.store.Load(ctx, auth.ID)
		if errLoad != nil {
			if ctx.Err() != nil {
				// Shutdown canceled the rebuild; do not log a spurious failure.
				return
			}
			log.Warnf("workbuddy tasks: load state for %s: %v", auth.ID, errLoad)
			state = State{}
		}
		if state.Version == 0 {
			state.Version = CurrentStateVersion
		}
		if state.AuthID == "" {
			state.AuthID = auth.ID
		}
		if state.Provider == "" {
			state.Provider = auth.Provider
		}
		if state.UID == "" {
			state.UID = authUID(auth)
		}
		if state.Tasks == nil {
			state.Tasks = make(map[string]TaskState)
		}
		loaded[auth.ID] = state
	}

	s.cleanupOrphans(ctx, wantedIDs)

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return
	}
	states := make(map[string]State, len(loaded))
	queue := make(taskHeap, 0, len(loaded))
	index := make(map[string]*taskHeapItem, len(loaded))
	for authID, diskState := range loaded {
		state := diskState
		// In-memory state may be newer than disk when a rebuild races with a
		// state write; never regress it.
		if existing, ok := s.states[authID]; ok && existing.UpdatedAt.After(state.UpdatedAt) {
			state = existing
		}
		states[authID] = state
		if next, ok := earliestNext(now, taskList, loc, state); ok {
			item := &taskHeapItem{authID: authID, next: next}
			heap.Push(&queue, item)
			index[authID] = item
		}
	}
	s.queue = queue
	s.index = index
	s.states = states
}

// cleanupOrphans deletes state of accounts that no longer exist and have not
// been updated for orphanStateRetention.
func (s *Scheduler) cleanupOrphans(ctx context.Context, wanted map[string]struct{}) {
	lister, ok := s.store.(StateLister)
	if !ok {
		return
	}
	states, errList := lister.List(ctx)
	if errList != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warnf("workbuddy tasks: list state files: %v", errList)
		return
	}
	now := s.now()
	for _, state := range states {
		if state.AuthID == "" {
			continue
		}
		if _, exists := wanted[state.AuthID]; exists {
			continue
		}
		if !state.UpdatedAt.IsZero() && now.Sub(state.UpdatedAt) <= orphanStateRetention {
			continue
		}
		if errDelete := s.store.Delete(ctx, state.AuthID); errDelete != nil {
			log.Warnf("workbuddy tasks: delete orphan state for %s: %v", state.AuthID, errDelete)
		}
	}
}

// persist writes state to the store. Failures are logged, never fatal.
func (s *Scheduler) persist(state State) {
	if s.store == nil {
		return
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if errSave := s.store.Save(context.Background(), state); errSave != nil {
		log.Warnf("workbuddy tasks: save state for %s: %v", state.AuthID, errSave)
	}
}

// removeAuth drops an account from the schedule and releases its lock entry.
// removeAuth drops an account from the schedule. The per-account lock entry is
// intentionally retained: an in-flight execute may still hold the old lock, and
// re-adding the same ID must not create a second lock that runs concurrently.
func (s *Scheduler) removeAuth(authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(authID)
}

func (s *Scheduler) removeLocked(authID string) {
	item, ok := s.index[authID]
	if !ok || item == nil {
		return
	}
	heap.Remove(&s.queue, item.index)
	delete(s.index, authID)
}

func (s *Scheduler) upsertLocked(authID string, next time.Time) {
	if authID == "" || next.IsZero() {
		return
	}
	if item, ok := s.index[authID]; ok && item != nil {
		item.next = next
		heap.Fix(&s.queue, item.index)
		return
	}
	item := &taskHeapItem{authID: authID, next: next}
	heap.Push(&s.queue, item)
	s.index[authID] = item
}

// popDue removes and returns every account due at now together with the
// generation they were scheduled under.
func (s *Scheduler) popDue(now time.Time) ([]string, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	generation := s.generation
	var due []string
	for len(s.queue) > 0 {
		item := s.queue[0]
		if item == nil || item.next.After(now) {
			break
		}
		popped := heap.Pop(&s.queue).(*taskHeapItem)
		if popped == nil {
			continue
		}
		delete(s.index, popped.authID)
		due = append(due, popped.authID)
	}
	return due, generation
}

// peek returns the earliest scheduled instant.
func (s *Scheduler) peek() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return time.Time{}, false
	}
	return s.queue[0].next, true
}

// resetTimer arms the timer for the next scheduled instant.
func (s *Scheduler) resetTimer(timer *time.Timer, timerCh *<-chan time.Time) {
	next, ok := s.peek()
	if !ok {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		*timerCh = nil
		return
	}
	wait := next.Sub(s.now())
	if wait < 0 {
		wait = 0
	}
	if wait > maxTimerWait {
		wait = maxTimerWait
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(wait)
	*timerCh = timer.C
}

// authLockLocked returns the per-account semaphore, creating it on first use.
func (s *Scheduler) authLockLocked(authID string) *authLock {
	if s.authLocks == nil {
		s.authLocks = make(map[string]*authLock)
	}
	lock, ok := s.authLocks[authID]
	if !ok || lock == nil {
		lock = newAuthLock()
		s.authLocks[authID] = lock
	}
	return lock
}

// authLock is a one-slot semaphore that serializes tasks of one account. It is
// channel based so waiting respects context cancellation.
type authLock struct {
	sem chan struct{}
}

func newAuthLock() *authLock {
	return &authLock{sem: make(chan struct{}, 1)}
}

// acquire blocks until the lock is free or ctx is canceled.
func (l *authLock) acquire(ctx context.Context) bool {
	select {
	case l.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// release frees the lock.
func (l *authLock) release() {
	<-l.sem
}

// backoff returns the retry delay for the given consecutive failure count.
func (s *Scheduler) backoff(failures int) time.Duration {
	delay := backoffDelay(failures)
	if s.jitter != nil {
		delay = s.jitter(delay)
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}

// backoffDelay maps a failure count to the base retry delay.
func backoffDelay(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures <= len(backoffSteps) {
		return backoffSteps[failures-1]
	}
	delay := backoffSteps[len(backoffSteps)-1]
	for i := len(backoffSteps); i < failures; i++ {
		delay *= 2
		if delay >= maxBackoff {
			return maxBackoff
		}
	}
	return delay
}

// jitterDuration spreads a delay by up to 10% in either direction.
func jitterDuration(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	spread := delay / 10
	if spread <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int64N(int64(spread)*2+1)) - spread
}

// nextTaskRun combines the spec schedule with a pending failure backoff,
// preferring whichever retry instant comes first.
func nextTaskRun(now time.Time, spec Spec, loc *time.Location, state TaskState) (time.Time, bool) {
	next, ok := NextDue(now, spec, loc, state.LastRun)
	if !ok {
		return time.Time{}, false
	}
	// A failed task retries on the backoff schedule; the normal cadence
	// resumes after a success. Use the earlier of the two so a short backoff is
	// not shadowed by a long interval.
	if state.ConsecutiveFailures > 0 && !state.NextRun.IsZero() && state.NextRun.Before(next) {
		return state.NextRun, true
	}
	return next, true
}

// earliestNext returns the earliest scheduled instant across the tasks.
func earliestNext(now time.Time, taskList []Task, loc *time.Location, state State) (time.Time, bool) {
	var next time.Time
	found := false
	for _, task := range taskList {
		spec := task.Spec()
		if !spec.Enabled {
			continue
		}
		if spec.Provider != "" && !strings.EqualFold(spec.Provider, state.Provider) {
			continue
		}
		candidate, ok := nextTaskRun(now, spec, loc, state.Tasks[spec.Name])
		if !ok {
			continue
		}
		if !found || candidate.Before(next) {
			next = candidate
			found = true
		}
	}
	return next, found
}

// authUID extracts the WorkBuddy account uid from an auth record.
func authUID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if uid := strings.TrimSpace(auth.Attributes["uid"]); uid != "" {
		return uid
	}
	if auth.Metadata != nil {
		if uid, ok := auth.Metadata["uid"].(string); ok {
			return strings.TrimSpace(uid)
		}
	}
	return ""
}

// ResolveStateDir returns the configured state directory or the default under
// the resolved auth directory.
func ResolveStateDir(cfg *internalconfig.Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("nil config")
	}
	if dir := strings.TrimSpace(cfg.WorkBuddy.StateDir); dir != "" {
		return dir, nil
	}
	authDir, errResolve := util.ResolveAuthDir(cfg.AuthDir)
	if errResolve != nil {
		return "", errResolve
	}
	return filepath.Join(authDir, "workbuddy-state"), nil
}

// resolveWorkers returns the configured worker count or the default. The cap
// also protects programmatic Config values that bypass YAML validation.
func resolveWorkers(cfg *internalconfig.Config) int {
	if cfg != nil && cfg.WorkBuddy.Workers > 0 {
		if cfg.WorkBuddy.Workers > maxWorkers {
			return maxWorkers
		}
		return cfg.WorkBuddy.Workers
	}
	return DefaultWorkers
}

// resolveTimeout parses the configured request timeout.
func resolveTimeout(cfg *internalconfig.Config) time.Duration {
	if cfg != nil {
		if raw := strings.TrimSpace(cfg.WorkBuddy.RequestTimeout); raw != "" {
			if timeout, errParse := time.ParseDuration(raw); errParse == nil && timeout > 0 {
				return timeout
			}
			log.Warnf("workbuddy tasks: invalid request-timeout %q; using %s", raw, defaultTaskTimeout)
		}
	}
	return defaultTaskTimeout
}

// defaultLocation is the fallback timezone for daily windows. WorkBuddy is
// CN-only, so Asia/Shanghai is the natural default; UTC is used only when the
// system timezone database is unavailable.
var defaultLocation = loadDefaultLocation()

func loadDefaultLocation() *time.Location {
	location, errLoad := time.LoadLocation("Asia/Shanghai")
	if errLoad != nil {
		return time.UTC
	}
	return location
}

// resolveLocation parses the configured timezone, falling back to
// defaultLocation (Asia/Shanghai) to match the WorkBuddy task client.
func resolveLocation(cfg *internalconfig.Config) *time.Location {
	if cfg != nil {
		if name := strings.TrimSpace(cfg.WorkBuddy.Timezone); name != "" {
			location, errLoad := time.LoadLocation(name)
			if errLoad == nil {
				return location
			}
			log.Warnf("workbuddy tasks: invalid timezone %q; using %s", name, defaultLocation)
		}
	}
	return defaultLocation
}

// taskHeapItem tracks the earliest due instant of one account.
type taskHeapItem struct {
	authID string
	next   time.Time
	index  int
}

// taskHeap is a min-heap ordered by next run time.
type taskHeap []*taskHeapItem

func (h taskHeap) Len() int { return len(h) }

func (h taskHeap) Less(i, j int) bool { return h[i].next.Before(h[j].next) }

func (h taskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *taskHeap) Push(x any) {
	item, ok := x.(*taskHeapItem)
	if !ok || item == nil {
		return
	}
	item.index = len(*h)
	*h = append(*h, item)
}

func (h *taskHeap) Pop() any {
	old := *h
	n := len(old)
	if n == 0 {
		return (*taskHeapItem)(nil)
	}
	item := old[n-1]
	item.index = -1
	*h = old[:n-1]
	return item
}
