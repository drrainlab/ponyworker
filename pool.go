package workers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	uuid "github.com/satori/go.uuid"
)

var (
	ErrPoolShuttingDown error = errors.New("pool is shutting down. No new jobs available")
	ErrPoolSizeExceeded error = errors.New("pool size exceeded")
	ErrWorkerNotFound   error = errors.New("worker with that key not found")
	ErrAlreadyRunning   error = errors.New("the job is already running")
)

type job func(interface{}) error
type JobStatus string
type stopReason string

const (
	JOB_QUEUED_STATUS     JobStatus = "queued"
	JOB_PROCESSING_STATUS JobStatus = "processing"
	JOB_STOPPED_STATUS    JobStatus = "stopped"
	JOB_FAILED_STATUS     JobStatus = "failed"
)

type Ponypool interface {
	HasWorker(key string) bool
	SpawnWorker(opts *JobOptions) error
	StopWorker(key interface{}, reason string) error
	ChangeWorkerInterval(key interface{}, interval time.Duration) error
	Shutdown()
}

type JobInfo struct {
	timeStarted     time.Time
	isStopping      uint32
	stopWG          *sync.WaitGroup
	stopChannel     chan stopReason
	intervalChannel chan time.Duration
	status          JobStatus
	err             error
}

type Pool struct {
	mu               *sync.RWMutex
	wg               *sync.WaitGroup
	maxSize          uint64
	curSize          uint64 // atomic workers counter
	hasActiveWorkers uint32 // atomic flag
	isStopping       uint32 // atomic flag
	jobChan          chan job
	cancelChan       chan struct{}
	jobInfoMap       map[jobKey]*JobInfo
}

type Worker struct {
	isStopping uint32 // atomic flag
}

type jobKey interface{}

type JobOptions struct {
	Key             jobKey // job key
	ScheduledToTime time.Time
	Mode            JobMode
	Data            interface{}
}

func NewPool(ctx context.Context, maxSize uint64) Ponypool {
	pool := &Pool{
		mu:         new(sync.RWMutex),
		maxSize:    maxSize,
		curSize:    0,
		jobChan:    make(chan job, maxSize),
		cancelChan: make(chan struct{}),
		jobInfoMap: map[jobKey]*JobInfo{},
	}

	pool.jobInfoMap = make(map[jobKey]*JobInfo, maxSize)
	pool.wg = new(sync.WaitGroup)
	atomic.StoreUint32(&pool.hasActiveWorkers, 0)
	atomic.StoreUint32(&pool.isStopping, 0)

	pool.wg.Add(1) // shutdown waiter
	return pool
}

func (p *Pool) HasWorker(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exist := p.jobInfoMap[key]
	return exist
}

func (p *Pool) isShuttingDown() bool {
	stopping := atomic.LoadUint32(&p.isStopping)
	return stopping == 1
}

func (p *Pool) Shutdown() {
	atomic.StoreUint32(&p.isStopping, 1)
	log.Debug().Msgf("Waiting for %d jobs to complete...", len(p.jobInfoMap))
	close(p.cancelChan)
	p.wg.Done()
	p.wg.Wait()
}

func (p *Pool) SpawnWorker(opts *JobOptions) error {
	if p.isShuttingDown() {
		return ErrPoolShuttingDown
	}
	// check if max workers size exceeded
	curSize := atomic.LoadUint64(&p.curSize)
	if curSize == p.maxSize {
		return ErrPoolSizeExceeded
	}
	jobInfo := p.jobInfoMap[opts.Key]
	// assign random key if not set
	if opts.Key == nil {
		opts.Key = uuid.NewV4()
	} else {
		if _, exist := p.jobInfoMap[opts.Key]; exist {
			// check if job actually running or in stopping phase
			if stopFlag := atomic.LoadUint32(&jobInfo.isStopping); stopFlag == 0 {
				return ErrAlreadyRunning
			}
			// wait job to be fully stopped
			jobInfo.stopWG.Wait()
		}
	}

	atomic.AddUint64(&p.curSize, 1)
	p.wg.Add(1)
	stopWG := new(sync.WaitGroup)
	// waitin for job to be fully stopped on same job simultaneously stop/start event
	stopWG.Add(1)
	p.mu.Lock()
	jobInfo = &JobInfo{
		stopChannel:     make(chan stopReason, 1),
		intervalChannel: make(chan time.Duration),
		isStopping:      0,
		stopWG:          stopWG,
		timeStarted:     time.Now(),
		status:          JOB_QUEUED_STATUS,
	}
	go p.process(opts)
	p.jobInfoMap[opts.Key] = jobInfo
	p.mu.Unlock()
	return nil
}

// StopWorker sends st
func (p *Pool) StopWorker(key interface{}, reason string) error {
	log.Info().Msgf("Stopping worker with key %v", key)
	p.mu.Lock()
	defer p.mu.Unlock()
	jobInfo, exist := p.jobInfoMap[key]
	if !exist {
		return ErrWorkerNotFound
	}
	atomic.StoreUint32(&jobInfo.isStopping, 1)
	jobInfo.status = JOB_STOPPED_STATUS
	jobInfo.stopChannel <- stopReason(reason)
	log.Info().Msgf("Worker with key %v stopped", key)
	return nil
}

func (p *Pool) ChangeWorkerInterval(key interface{}, interval time.Duration) error {
	log.Info().Msgf("Changing worker interval | key = %v", key)
	p.mu.Lock()
	defer p.mu.Unlock()
	jobInfo, exist := p.jobInfoMap[key]
	if !exist {
		return ErrWorkerNotFound
	}
	jobInfo.intervalChannel <- interval
	log.Info().Msgf("Worker with key %v changed interval", key)
	return nil
}

func (p *Pool) process(opts *JobOptions) {
	jobInfo := p.jobInfoMap[opts.Key]
	jobInfo.status = JOB_PROCESSING_STATUS

	fnChan := make(chan func() error)
	ctx, cancel := context.WithCancel(context.Background())
	go opts.Mode.start(ctx, fnChan)

ProcessLoop:
	for {
		select {
		case execFn := <-fnChan:
			go func() {
				if err := execFn(); err != nil {
					log.Error().Stack().Msg(err.Error())
					jobInfo.err = err
					jobInfo.status = JOB_FAILED_STATUS
				}
			}()
		case <-p.cancelChan:
			cancel()
			jobInfo.status = JOB_STOPPED_STATUS
			break ProcessLoop
		case reason := <-jobInfo.stopChannel:
			log.Info().Msgf("Worker %v got stop signal: reason = %v", opts.Key, reason)
			cancel()
			jobInfo.status = JOB_STOPPED_STATUS
			break ProcessLoop
		}
	}

	log.Info().Msgf("Stopping individual worker with key %v", opts.Key)
	jobInfo.status = JOB_STOPPED_STATUS
	p.cleanAfterJob(opts.Key)
	p.wg.Done()
}

func (p *Pool) cleanAfterJob(key interface{}) {
	size := atomic.LoadUint64(&p.curSize)
	atomic.StoreUint64(&p.curSize, size-1)
	p.mu.Lock()
	close(p.jobInfoMap[key].stopChannel)
	close(p.jobInfoMap[key].intervalChannel)
	p.jobInfoMap[key].stopWG.Done()
	delete(p.jobInfoMap, key)
	p.mu.Unlock()
}
