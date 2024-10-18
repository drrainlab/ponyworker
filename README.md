# ponyworker
Customized worker pool supporting scheduler and on/off func execution

## API Reference

### SpawnWorker

SpawnWorker(opts *JobOptions) error

Spawns a new worker with the provided job options. Returns an error if the worker creation fails.

Example:

```go
pool := ponyworker.New()
err := pool.SpawnWorker(&ponyworker.JobOptions{
    Key:      "worker1",
    Interval: time.Second * 5,
    Job:      func() { fmt.Println("Worker 1 executing") },
})
if err != nil {
    log.Fatalf("Failed to spawn worker: %v", err)
}
```


### StopWorker

StopWorker(key interface{}, reason string) error

Stops a worker identified by the given key with the specified reason. Returns an error if the worker doesn't exist or cannot be stopped.

Example:

```go
pool := ponyworker.New()
err := pool.StopWorker("worker1", "Task completed")
if err != nil {
    log.Printf("Failed to stop worker: %v", err)
}
```

### ChangeWorkerInterval

ChangeWorkerInterval(key interface{}, interval time.Duration) error

Changes the execution interval of a worker identified by the given key. Returns an error if the worker doesn't exist or the interval change fails.

Example:

```go
pool := ponyworker.New()
err := pool.ChangeWorkerInterval("worker1", time.Minute)
if err != nil {
    log.Printf("Failed to change worker interval: %v", err)
}
```

### Shutdown
Shuts down the entire worker pool, stopping all workers gracefully.

Example:

```go
pool := ponyworker.New()
// ... spawn workers and perform operations
pool.Shutdown()
fmt.Println("Worker pool has been shut down")
```

# Ponyworker Modes

Ponyworker offers various execution modes for flexible job scheduling and execution patterns:

1. OnceMode: Executes a job once after a specified wait time.
2. RepeatMode: Executes a job repeatedly at a specified interval.
3. CyclicMode: Alternates between "on" and "off" states, executing different functions for each state.
4. ScheduleMode: Executes jobs based on a complex schedule defined by a table of time offsets and durations.

## Usage and Examples

### OnceMode

```go
onceMode := &OnceMode{
    WaitTimeSecs: 5 * time.Second,
    Fn: func(ctx context.Context, estimateTimeSecs int64) func() error {
        return func() error {
            fmt.Println("Executing once after 5 seconds")
            return nil
        }
    },
}
```

### RepeatMode

```go
repeatMode := &RepeatMode{
    RepeatInterval: 10 * time.Second,
    Fn: func(ctx context.Context, estimateTimeSecs int64) func() error {
        return func() error {
            fmt.Println("Executing every 10 seconds")
            return nil
        }
    },
}
```

### CyclicMode

```go
cyclicMode := &CyclicMode{
    Location:    time.UTC,
    DateBegin:   time.Now(),
    OnDuration:  30 * time.Second,
    OffDuration: 30 * time.Second,
    OnFunc: func(ctx context.Context, estimateTimeSecs int64) func() error {
        return func() error {
            fmt.Println("On state for 30 seconds")
            return nil
        }
    },
    OffFunc: func(ctx context.Context, estimateTimeSecs int64) func() error {
        return func() error {
            fmt.Println("Off state for 30 seconds")
            return nil
        }
    },
}
```

### ScheduleMode

```go
scheduleMode := &ScheduleMode{
    Location: time.UTC,
    OnFunc: func(ctx context.Context, estimateTimeSecs int64) func() error {
        return func() error {
            fmt.Println("Schedule: On state")
            return nil
        }
    },
    OffFunc: func(ctx context.Context, estimateTimeSecs int64) func() error {
        return func() error {
            fmt.Println("Schedule: Off state")
            return nil
        }
    },
    Schedule: Schedule{
        Table: []ScheduleTableItem{
            {TimeOffset: 3600, OnTime: 1800},  // Start at 1:00, run for 30 minutes
            {TimeOffset: 7200, OnTime: 3600},  // Start at 2:00, run for 1 hour
        },
        Dbegin: time.Now().Unix(),
        Dskip:  1,  // Skip 1 day between cycles
    },
}
```

Example:

```go
pool := ponyworker.New()
err := pool.SpawnWorker(&ponyworker.JobOptions{
    Key:  "worker1",
    Mode: onceMode,  // or repeatMode, cyclicMode, scheduleMode
})
if err != nil {
    log.Fatalf("Failed to spawn worker: %v", err)
}
```