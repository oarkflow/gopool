package gopool

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

type Job[I any] struct {
	Payload I
	ID      int
	Attempt int
}

type Result[I, O any] struct {
	Job   Job[I]
	Value O
	Error error
}

type Pool[I, O, C any] struct {
	numOfWorkers             int
	maxActiveWorkers         int
	fixedWorkers             bool
	workerStopAfterNumOfJobs int
	workerStopDurMultiplier  float64
	retries                  int
	reinitDelay              time.Duration
	targetLoad               float64
	name                     string
	loggerInfo               *log.Logger
	loggerDebug              *log.Logger
	handler                  func(job Job[I], workerID int, connection C) (O, error)
	workerInit               func(workerID int) (C, error)
	workerDeinit             func(workerID int, connection C) error
	concurrency              int32
	jobsNew                  chan I
	jobsQueue                chan Job[I]
	wgJobs                   sync.WaitGroup
	wgWorkers                sync.WaitGroup
	nJobsProcessing          int32
	jobsDone                 chan Result[I, O]
	// If the pool is created by the constructors
	// NewPoolWithResults() or NewPoolWithResultsAndInit(),
	// results are written to this channel,
	// and you must consume from this channel in a loop until it is closed.
	// If the pool is created by the constructors
	// NewPoolSimple() or NewPoolWithInit(),
	// this channel is nil.
	Results          chan Result[I, O]
	enableWorker     chan int
	disableWorker    chan struct{}
	monitor          func(s stats)
	cancelWorkers    context.CancelFunc
	loopDone         chan struct{}
	enableLoopDone   chan struct{}
	sleepingWorkers  int
	stoppedWorkers   ring[*worker[I, O, C]]
	stoppedWorkersMu sync.Mutex // protects: stoppedWorkers and sleepingWorkers
	waitCtx          context.Context
	waitCtxCancel    context.CancelFunc
}

// NewPoolSimple creates a new worker pool.
func NewPoolSimple[I any](numOfWorkers int, handler func(job Job[I], workerID int) error, options ...func(*poolConfig) error) (*Pool[I, struct{}, struct{}], error) {
	handler2 := func(job Job[I], workerID int, connection struct{}) error {
		return handler(job, workerID)
	}
	return NewPoolWithInit[I, struct{}](numOfWorkers, handler2, nil, nil, options...)
}

// NewPoolWithInit creates a new worker pool with workerInit() and workerDeinit() functions.
func NewPoolWithInit[I, C any](numOfWorkers int, handler func(job Job[I], workerID int, connection C) error, workerInit func(workerID int) (C, error), workerDeinit func(workerID int, connection C) error, options ...func(*poolConfig) error) (*Pool[I, struct{}, C], error) {
	handler2 := func(job Job[I], workerID int, connection C) (struct{}, error) {
		return struct{}{}, handler(job, workerID, connection)
	}
	return newPool[I, struct{}, C](numOfWorkers, handler2, workerInit, workerDeinit, false, options...)
}

// NewPoolWithResults creates a new worker pool with Results channel.
// You must consume from this channel in a loop until it is closed.
func NewPoolWithResults[I, O any](numOfWorkers int, handler func(job Job[I], workerID int) (O, error), options ...func(*poolConfig) error) (*Pool[I, O, struct{}], error) {
	handler2 := func(job Job[I], workerID int, connection struct{}) (O, error) {
		return handler(job, workerID)
	}
	return newPool[I, O, struct{}](numOfWorkers, handler2, nil, nil, true, options...)
}

// NewPoolWithResultsAndInit creates a new worker pool with workerInit() and workerDeinit() functions and Results channel.
// You must consume from this channel in a loop until it is closed.
func NewPoolWithResultsAndInit[I, O, C any](numOfWorkers int, handler func(job Job[I], workerID int, connection C) (O, error), workerInit func(workerID int) (C, error), workerDeinit func(workerID int, connection C) error, options ...func(*poolConfig) error) (*Pool[I, O, C], error) {
	return newPool[I, O, C](numOfWorkers, handler, workerInit, workerDeinit, true, options...)
}

func newPool[I, O, C any](numOfWorkers int, handler func(job Job[I], workerID int, connection C) (O, error), workerInit func(workerID int) (C, error), workerDeinit func(workerID int, connection C) error, createResultsChannel bool, options ...func(*poolConfig) error) (*Pool[I, O, C], error) {
	// default configuration
	config := poolConfig{
		setOptions:       make(map[int]struct{}),
		maxActiveWorkers: numOfWorkers,
		retries:          1,
		reinitDelay:      time.Second,
		targetLoad:       0.9,
	}
	for _, option := range options {
		err := option(&config)
		if err != nil {
			return nil, fmt.Errorf("config error: %s", err)
		}
	}
	_, setFixedWorkers := config.setOptions[optionFixedWorkers]
	_, setTargetLoad := config.setOptions[optionTargetLoad]
	if setFixedWorkers && setTargetLoad {
		return nil, fmt.Errorf("options FixedWorkers() and TargetLoad() are incompatible")
	}

	var loggerInfo *log.Logger
	if config.loggerInfo != nil {
		if config.name == "" {
			loggerInfo = config.loggerInfo
		} else {
			loggerInfo = log.New(config.loggerInfo.Writer(), config.loggerInfo.Prefix()+"[pool="+config.name+"] ", config.loggerInfo.Flags()|log.Lmsgprefix)
		}
	}
	var loggerDebug *log.Logger
	if config.loggerDebug != nil {
		if config.name == "" {
			loggerDebug = config.loggerDebug
		} else {
			loggerDebug = log.New(config.loggerDebug.Writer(), config.loggerDebug.Prefix()+"[pool="+config.name+"] ", config.loggerDebug.Flags()|log.Lmsgprefix)
		}
	}

	ctxActive, cancelActive := context.WithCancel(context.Background())
	ctxWorkers, cancelWorkers := context.WithCancel(context.Background())

	p := Pool[I, O, C]{
		retries:                  config.retries,
		reinitDelay:              config.reinitDelay,
		targetLoad:               config.targetLoad,
		name:                     config.name,
		loggerInfo:               loggerInfo,
		loggerDebug:              loggerDebug,
		monitor:                  config.monitor,
		numOfWorkers:             numOfWorkers,
		maxActiveWorkers:         config.maxActiveWorkers,
		fixedWorkers:             config.fixedWorkers,
		workerStopAfterNumOfJobs: config.workerStopAfterNumOfJobs,
		workerStopDurMultiplier:  config.workerStopDurMultiplier,
		handler:                  handler,
		workerInit:               workerInit,
		workerDeinit:             workerDeinit,
		cancelWorkers:            cancelWorkers,
		waitCtx:                  ctxActive,
		waitCtxCancel:            cancelActive,
	}
	if p.numOfWorkers <= 0 {
		return nil, fmt.Errorf("numOfWorkers <= 0")
	}
	if p.maxActiveWorkers <= 0 {
		return nil, fmt.Errorf("maxActiveWorkers <= 0")
	}
	if p.maxActiveWorkers > p.numOfWorkers {
		return nil, fmt.Errorf("maxActiveWorkers > numOfWorkers")
	}
	p.jobsNew = make(chan I, 2)
	p.jobsQueue = make(chan Job[I], p.maxActiveWorkers) // size p.maxActiveWorkers in order to avoid deadlock
	p.jobsDone = make(chan Result[I, O], p.maxActiveWorkers)
	if createResultsChannel {
		p.Results = make(chan Result[I, O], p.maxActiveWorkers)
	}
	p.enableWorker = make(chan int, p.maxActiveWorkers)
	p.disableWorker = make(chan struct{}, p.maxActiveWorkers)
	p.stoppedWorkers = newRing[*worker[I, O, C]](p.numOfWorkers)
	p.loopDone = make(chan struct{})
	p.enableLoopDone = make(chan struct{})
	for i := 0; i < p.numOfWorkers; i++ {
		w := newWorker(&p, i, ctxWorkers)
		ringPush(&p.stoppedWorkers, w)
	}
	p.stoppedWorkers.Shuffle()
	go p.enableWorkersLoop(ctxWorkers)
	go p.loop()
	return &p, nil
}

type stats struct {
	Time          time.Time
	NJobsInSystem int
	Concurrency   int32
	JobID         int
	DoneCounter   int
}

// loop at each iteration:
// - updates stats
// - handles auto-scaling
// - receives a submitted job payload from p.jobsNew OR receive a done job from p.jobsDone
func (p *Pool[I, O, C]) loop() {
	var loadAvg float64 = 1
	var lenResultsAVG float64
	var jobID int
	var jobIDWhenLastEnabledWorker int
	var doneCounter int
	var doneCounterWhenDisabledWorker int
	var doneCounterWhenDisabledWorkerResultsBlocked int
	var nJobsInSystem int
	var jobDone bool
	var resultsBlocked bool

	// concurrencyThreshold stores the value of p.concurrency at the last time write to the p.Results channel blocked
	concurrencyThreshold := int32(p.maxActiveWorkers)

	// calculate decay factor "a"
	// of the exponentially weighted moving average.
	// first we calculate the "window" (formula found experimentaly),
	// and then "a" using the formula a = 2/(window+1) (commonly used formula for a)
	window := p.maxActiveWorkers / 2
	if window < 5 {
		window = 5
	}
	a := 2 / (float64(window) + 1)

	// window2 is used as the number of jobs that have to pass
	// before we enable or disable workers again
	window2 := window

	// initialize with negative value so we don't wait 'window2' submissions for auto-scaling to run for the 1st time
	jobIDWhenLastEnabledWorker = -window2 / 2
	doneCounterWhenDisabledWorker = -window2 / 2

	for p.jobsNew != nil || p.jobsDone != nil {
		// update stats
		lenResultsAVG = a*float64(len(p.Results)) + (1-a)*lenResultsAVG
		concurrency := atomic.LoadInt32(&p.concurrency)
		if concurrency > 0 {
			// concurrency > 0 so we can divide
			loadNow := float64(nJobsInSystem) / float64(concurrency)
			loadAvg = a*loadNow + (1-a)*loadAvg
			if p.loggerDebug != nil {
				nJobsProcessing := atomic.LoadInt32(&p.nJobsProcessing)
				if jobDone {
					p.loggerDebug.Printf("[workerpool/loop] len(jobsNew)=%d len(jobsQueue)=%d lenResultsAVG=%.2f nJobsProcessing=%d nJobsInSystem=%d concurrency=%d loadAvg=%.2f doneCounter=%d\n", len(p.jobsNew), len(p.jobsQueue), lenResultsAVG, nJobsProcessing, nJobsInSystem, concurrency, loadAvg, doneCounter)
				} else {
					p.loggerDebug.Printf("[workerpool/loop] len(jobsNew)=%d len(jobsQueue)=%d lenResultsAVG=%.2f nJobsProcessing=%d nJobsInSystem=%d concurrency=%d loadAvg=%.2f jobID=%d\n", len(p.jobsNew), len(p.jobsQueue), lenResultsAVG, nJobsProcessing, nJobsInSystem, concurrency, loadAvg, jobID)
				}
			}
		} else {
			loadAvg = 1
		}
		if p.monitor != nil {
			p.monitor(stats{
				Time:          time.Now(),
				NJobsInSystem: nJobsInSystem,
				Concurrency:   concurrency,
				JobID:         jobID,
				DoneCounter:   doneCounter,
			})
		}

		if p.fixedWorkers {
			concurrencyDesired := p.maxActiveWorkers
			concurrencyDiff := int32(concurrencyDesired) - concurrency
			if concurrencyDiff > 0 {
				if p.loggerDebug != nil {
					p.loggerDebug.Printf("[workerpool/loop] [jobID=%d] enabling %d new workers", jobID, concurrencyDiff)
				}
				p.enableWorkers(concurrencyDiff)
			}
		} else {
			// if this is not a pool with fixed number of workers, run auto-scaling
			if !jobDone && // if we received a new job in the previous iteration
				loadAvg/p.targetLoad > float64(concurrency+1)/float64(concurrency) && // and load is high
				jobID-jobIDWhenLastEnabledWorker > window2 && // and we haven't enabled a worker recently
				len(p.Results) == 0 { // and there is no backpressure
				// calculate desired concurrency
				// concurrencyDesired/concurrency = loadAvg/p.targetLoad
				concurrencyDesired := float64(concurrency) * loadAvg / p.targetLoad
				// reduce desired concurrency if it exceeds threshold
				concurrencyExcess := concurrencyDesired - float64(concurrencyThreshold)
				if concurrencyExcess > 0 {
					concurrencyDesired = float64(concurrencyThreshold) + 0.3*concurrencyExcess
				}
				concurrencyDiffFloat := concurrencyDesired - float64(concurrency)
				if p.Results != nil {
					// then we multiply by 1-sqrt(lenResultsAVG/cap(p.Results)) (found experimentally. needs improvement)
					// in order to reduce concurrencyDiff if there is backpressure and len(p.Results) == 0 was temporary
					concurrencyDiffFloat *= 1 - math.Sqrt(lenResultsAVG/float64(cap(p.Results)))
				}
				concurrencyDiff := int32(math.Round(concurrencyDiffFloat))
				if concurrencyDiff > 0 {
					if p.loggerDebug != nil {
						p.loggerDebug.Printf("[workerpool/loop] [jobID=%d] high load - enabling %d new workers", jobID, concurrencyDiff)
					}
					p.enableWorkers(concurrencyDiff)
					jobIDWhenLastEnabledWorker = jobID
				}
			}
			if jobDone && // if a job was done in the previous iteration
				concurrency > 0 &&
				loadAvg/p.targetLoad < float64(concurrency-1)/float64(concurrency) && // and load is low
				doneCounter-doneCounterWhenDisabledWorker > window2 { // and we haven't disabled a worker recently
				// calculate desired concurrency
				// concurrencyDesired/concurrency = loadAvg/p.targetLoad
				concurrencyDesired := float64(concurrency) * loadAvg / p.targetLoad
				if int(math.Round(concurrencyDesired)) <= 0 {
					concurrencyDesired = 1
				}
				concurrencyDiff := int32(math.Round(concurrencyDesired)) - concurrency
				if concurrencyDiff < 0 {
					if p.loggerDebug != nil {
						p.loggerDebug.Printf("[workerpool/loop] [doneCounter=%d] low load - disabling %v workers", doneCounter, -concurrencyDiff)
					}
					p.disableWorkers(-concurrencyDiff)
					doneCounterWhenDisabledWorker = doneCounter
				}
			}
			if resultsBlocked && // if write to p.Results channel blocked in the previous iteration
				doneCounter-doneCounterWhenDisabledWorkerResultsBlocked > window2 { // and we haven't recently disabled a worker due to p.Results blocking
				concurrencyThreshold = concurrency
				concurrencyDesired := 0.9 * float64(concurrency)
				if int(math.Round(concurrencyDesired)) <= 0 {
					concurrencyDesired = 1
				}
				concurrencyDiff := int32(math.Round(concurrencyDesired)) - concurrency
				if concurrencyDiff < 0 {
					if p.loggerDebug != nil {
						p.loggerDebug.Printf("[workerpool/loop] [doneCounter=%d] write to p.Results blocked. try to disable %d workers\n", doneCounter, -concurrencyDiff)
					}
					p.disableWorkers(-concurrencyDiff)
					doneCounterWhenDisabledWorkerResultsBlocked = doneCounter
				}
			}
			// make sure not all workers are disabled while there are jobs
			if concurrency == 0 && nJobsInSystem > 0 {
				if p.loggerDebug != nil {
					p.loggerDebug.Printf("[workerpool/loop] [doneCounter=%d] no active worker. try to enable new worker", doneCounter)
				}
				p.enableWorkers(1)
			}
		}

		jobDone = false
		resultsBlocked = false
		if nJobsInSystem >= p.maxActiveWorkers {
			// If there are p.maxActiveWorkers jobs in the system, receive a done job from p.jobsDone, but don't accept new jobs.
			// That way we make sure nJobsInSystem < p.maxActiveWorkers
			// thus nJobsInSystem < cap(p.jobsQueue) (because cap(p.jobsQueue) = p.maxActiveWorkers)
			// thus p.jobsQueue cannot exceed it's capacity,
			// so writes to p.jobsQueue don't block.
			// Blocking writes to p.jobsQueue would cause deadlock.
			result, ok := <-p.jobsDone
			if !ok {
				p.jobsDone = nil
				continue
			}
			if p.Results != nil {
				select {
				case p.Results <- result:
				default:
					p.Results <- result
					resultsBlocked = true
				}
			}
			nJobsInSystem--
			doneCounter++
			jobDone = true
		} else {
			// receive a submitted job payload from p.jobsNew OR receive a done job from p.jobsDone
			select {
			case payload, ok := <-p.jobsNew:
				if !ok {
					p.jobsNew = nil
					continue
				}
				nJobsInSystem++
				p.jobsQueue <- Job[I]{Payload: payload, ID: jobID, Attempt: 0}
				jobID++
			case result, ok := <-p.jobsDone:
				if !ok {
					p.jobsDone = nil
					continue
				}
				if p.Results != nil {
					select {
					case p.Results <- result:
					default:
						p.Results <- result
						resultsBlocked = true
					}
				}
				nJobsInSystem--
				doneCounter++
				jobDone = true
			}
		}
	}
	close(p.jobsQueue)
	if p.loggerDebug != nil {
		p.loggerDebug.Println("[workerpool/loop] finished")
	}
	p.loopDone <- struct{}{}
}

func (p *Pool[I, O, C]) disableWorkers(n int32) {
	// drain p.enableWorker channel
loop:
	for {
		select {
		case <-p.enableWorker:
		default:
			break loop
		}
	}
	// try to disable n workers
	for i := int32(0); i < n; i++ {
		select {
		case p.disableWorker <- struct{}{}:
		default:
		}
	}
}

func (p *Pool[I, O, C]) enableWorkers(n int32) {
	// drain p.disableWorker channel
loop:
	for {
		select {
		case <-p.disableWorker:
		default:
			break loop
		}
	}
	// try to enable n workers
	select {
	case p.enableWorker <- int(n):
	default:
	}
}

func (p *Pool[I, O, C]) enableWorkersLoop(ctx context.Context) {
	defer func() {
		p.enableLoopDone <- struct{}{}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-p.enableWorker:
			if !ok {
				return
			}
			p.enableWorkers2(n)
		}
	}
}

func (p *Pool[I, O, C]) enableWorkers2(n int) {
	p.stoppedWorkersMu.Lock()
	defer p.stoppedWorkersMu.Unlock()

	if p.stoppedWorkers.Count == 0 {
		return
	}

	concurrency := p.numOfWorkers - p.stoppedWorkers.Count - p.sleepingWorkers
	if concurrency+n > p.maxActiveWorkers {
		n = p.maxActiveWorkers - concurrency
	}

	if n <= 0 {
		return
	}

	workersToStart := make([]*worker[I, O, C], 0, n)
	for i := 0; i < n; i++ {
		w, ok := ringPop(&p.stoppedWorkers)
		if !ok {
			break
		}
		workersToStart = append(workersToStart, w)
	}
	p.wgWorkers.Add(len(workersToStart))
	for _, workerToStart := range workersToStart {
		go workerToStart.loop()
	}
}

// Submit adds a new job to the queue.
func (p *Pool[I, O, C]) Submit(jobPayload I) {
	p.wgJobs.Add(1)
	p.jobsNew <- jobPayload
}

// StopAndWait shuts down the pool.
// Once called no more jobs can be submitted,
// and waits for all enqueued jobs to finish and workers to stop.
func (p *Pool[I, O, C]) StopAndWait() {
	close(p.jobsNew)
	if p.loggerDebug != nil {
		p.loggerDebug.Println("[workerpool/StopAndWait] waiting for all jobs to finish")
	}
	p.wgJobs.Wait()
	p.cancelWorkers()
	close(p.jobsDone)
	if p.loggerDebug != nil {
		p.loggerDebug.Println("[workerpool/StopAndWait] waiting for pool loop to finish")
	}
	<-p.loopDone
	if p.loggerDebug != nil {
		p.loggerDebug.Println("[workerpool/StopAndWait] waiting for all workers to finish")
	}
	p.wgWorkers.Wait()
	close(p.disableWorker)
	close(p.enableWorker)
	if p.loggerDebug != nil {
		p.loggerDebug.Println("[workerpool/StopAndWait] waiting for enableWorkersLoop to finish")
	}
	<-p.enableLoopDone
	if p.loggerDebug != nil {
		p.loggerDebug.Println("[workerpool/StopAndWait] finished")
	}
	if p.Results != nil {
		close(p.Results)
	}
	p.stoppedWorkersMu.Lock()
	defer p.stoppedWorkersMu.Unlock()
	p.stoppedWorkers.buffer = nil
	p.waitCtxCancel()
}

// Wait waits until the pool has been stopped by a call to StopAndWait()
func (p *Pool[I, O, C]) Wait() {
	<-p.waitCtx.Done()
}

// ConnectPools starts a goroutine that reads the results of the first pool,
// and submits them to the second, if there is no error,
// or passes them to the handleError function if there is an error.
//
// Once you call StopAndWait() on the first pool,
// after a while p1.Results get closed, and p2.StopAndWait() is called.
// This way StopAndWait() propagates through the pipeline.
//
// WARNING: Should only be used if the first pool has a not-nil Results channel.
// Which means it was created by the constructors NewPoolWithResults() or NewPoolWithResultsAndInit().
func ConnectPools[I, O, C, O2, C2 any](p1 *Pool[I, O, C], p2 *Pool[O, O2, C2], handleError func(Result[I, O])) {
	go func() {
		for result := range p1.Results {
			if result.Error != nil {
				if handleError != nil {
					handleError(result)
				}
			} else {
				p2.Submit(result.Value)
			}
		}
		p2.StopAndWait()
	}()
}

type worker[I, O, C any] struct {
	id         int
	pool       *Pool[I, O, C]
	ctx        context.Context
	connection *C
}

func newWorker[I, O, C any](p *Pool[I, O, C], id int, ctx context.Context) *worker[I, O, C] {
	return &worker[I, O, C]{
		id:   id,
		pool: p,
		ctx:  ctx,
	}
}

func (w *worker[I, O, C]) loop() {
	enabled := false
	slept := false
	deinit := func() {
		if enabled {
			enabled = false
			atomic.AddInt32(&w.pool.concurrency, -1)
			if w.pool.workerDeinit != nil && w.connection != nil {
				err := w.pool.workerDeinit(w.id, *w.connection)
				if err != nil {
					if w.pool.loggerInfo != nil {
						w.pool.loggerInfo.Printf("[workerpool/worker%d] workerDeinit failed: %s\n", w.id, err)
					}
				}
				w.connection = nil
			}
			concurrency := atomic.LoadInt32(&w.pool.concurrency)
			if w.pool.loggerDebug != nil {
				w.pool.loggerDebug.Printf("[workerpool/worker%d] worker disabled - concurrency %d\n", w.id, concurrency)
			}
		}
	}
	defer func() {
		if w.pool.loggerDebug != nil {
			w.pool.loggerDebug.Printf("[workerpool/worker%d] stopped\n", w.id)
		}

		deinit()

		// save this worker to w.pool.stoppedWorkers
		w.pool.stoppedWorkersMu.Lock()
		defer w.pool.stoppedWorkersMu.Unlock()
		if slept {
			w.pool.sleepingWorkers--
		}
		if w.pool.stoppedWorkers.buffer != nil { // might have been set to nil by pool.StopAndWait()
			ringPush(&w.pool.stoppedWorkers, w)
			concurrency := atomic.LoadInt32(&w.pool.concurrency)
			if concurrency == 0 {
				if w.pool.loggerDebug != nil {
					w.pool.loggerDebug.Printf("[workerpool/worker%d] enabling a worker because concurrency=0\n", w.id)
				}
				w.pool.enableWorkers(1)
			}
		}

		w.pool.wgWorkers.Done()
	}()

	select {
	case <-w.ctx.Done():
		if w.pool.loggerDebug != nil {
			w.pool.loggerDebug.Printf("ctx has been cancelled. worker cannot start")
		}
		return
	default:
	}
	enabled = true
	atomic.AddInt32(&w.pool.concurrency, 1)
	if w.pool.loggerDebug != nil {
		w.pool.loggerDebug.Printf("[workerpool/worker%d] worker enabled\n", w.id)
	}
	if w.pool.workerInit != nil {
		connection, err := w.pool.workerInit(w.id)
		if err != nil {
			if w.pool.loggerInfo != nil {
				w.pool.loggerInfo.Printf("[workerpool/worker%d] workerInit failed: %s\n", w.id, err)
			}
			enabled = false
			atomic.AddInt32(&w.pool.concurrency, -1)
			time.Sleep(w.pool.reinitDelay)
			// this worker failed to start, so enable another worker
			w.pool.enableWorkers(1)
			return
		}
		w.connection = &connection
	} else {
		w.connection = new(C)
	}

	started := time.Now()

	for i := 0; ; i++ {
		if w.pool.workerStopAfterNumOfJobs > 0 && i >= w.pool.workerStopAfterNumOfJobs {
			// enable another worker so concurrency does not decrease
			w.pool.enableWorkers(1)
			if w.pool.workerStopDurMultiplier > 0 {
				deinit()
				dur := time.Duration(float64(time.Since(started)) * w.pool.workerStopDurMultiplier)
				if w.pool.loggerDebug != nil {
					w.pool.loggerDebug.Printf("[workerpool/worker%v] completed %v jobs. active for %v sleeping for %v before stopping\n", w.id, i, time.Since(started), dur)
				}

				w.pool.stoppedWorkersMu.Lock()
				w.pool.sleepingWorkers++
				w.pool.stoppedWorkersMu.Unlock()
				slept = true
				sleepCtx(w.ctx, dur)
			} else if w.pool.loggerDebug != nil {
				w.pool.loggerDebug.Printf("[workerpool/worker%v] completed %v jobs. stopping\n", w.id, i)
			}
			return
		}
		select {
		case <-w.ctx.Done():
			return
		case _, ok := <-w.pool.disableWorker:
			if !ok {
				return
			}
			return
		case j, ok := <-w.pool.jobsQueue:
			if !ok {
				return
			}
			// run job
			atomic.AddInt32(&w.pool.nJobsProcessing, 1)
			resultValue, err := w.pool.handler(j, w.id, *w.connection)
			atomic.AddInt32(&w.pool.nJobsProcessing, -1)
			if err != nil && errorIsRetryable(err) && (j.Attempt < w.pool.retries || errorIsUnaccounted(err)) {
				// if error is retryable, put the job back in queue
				if !errorIsUnaccounted(err) {
					j.Attempt++
				}
				w.pool.jobsQueue <- j
			} else {
				// else job is done
				w.pool.jobsDone <- Result[I, O]{Job: j, Value: resultValue, Error: err}
				w.pool.wgJobs.Done()
			}
			// check if worker has to pause due to the error
			if err != nil {
				pauseDuration := errorPausesWorker(err)
				if pauseDuration > 0 {
					deinit()
					// enable another worker so concurrency does not decrease
					w.pool.enableWorkers(1)

					w.pool.stoppedWorkersMu.Lock()
					w.pool.sleepingWorkers++
					w.pool.stoppedWorkersMu.Unlock()
					slept = true
					sleepCtx(w.ctx, pauseDuration)
					return
				}
			}
		}
	}
}

func sleepCtx(ctx context.Context, dur time.Duration) bool {
	ticker := time.NewTicker(dur)
	defer ticker.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-ticker.C:
		return false
	}
}
