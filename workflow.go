package workflow

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/luno/jettison/errors"
	"github.com/luno/jettison/j"
	"github.com/luno/jettison/log"
	"k8s.io/utils/clock"

	"github.com/luno/workflow/internal/metrics"
)

type API[Type any, Status StatusType] interface {
	// Trigger will kickstart a workflow for the provided foreignID starting from the provided starting status. There
	// is no limitation as to where you start the workflow from. For workflows that have data preceding the initial
	// trigger that needs to be used in the workflow, using WithInitialValue will allow you to provide pre-populated
	// fields of Type that can be accessed by the consumers.
	//
	// foreignID should not be random and should be deterministic for the thing that you are running the workflow for.
	// This especially helps when connecting other workflows as the foreignID is the only way to connect the streams. The
	// same goes for Callback as you will need the foreignID to connect the callback back to the workflow instance that
	// was run.
	Trigger(ctx context.Context, foreignID string, startingStatus Status, opts ...TriggerOption[Type, Status]) (runID string, err error)

	// ScheduleTrigger takes a cron spec and will call Trigger at the specified intervals. ScheduleTrigger is a blocking
	// call and will return ErrWorkflowNotRunning or ErrStatusProvidedNotConfigured to indicate that it cannot begin to
	// schedule. All schedule errors will be retried indefinitely. The same options are available for ScheduleTrigger
	// as they are for Trigger.
	ScheduleTrigger(ctx context.Context, foreignID string, startingStatus Status, spec string, opts ...TriggerOption[Type, Status]) error

	// Await is a blocking call that returns the typed Record when the workflow of the specified run ID reaches the
	// specified status.
	Await(ctx context.Context, foreignID, runID string, status Status, opts ...AwaitOption) (*Record[Type, Status], error)

	// Callback can be used if Builder.AddCallback has been defined for the provided status. The data in the reader
	// will be passed to the CallbackFunc that you specify and so the serialisation and deserialisation is in the
	// hands of the user.
	Callback(ctx context.Context, foreignID string, status Status, payload io.Reader) error

	// Run must be called in order to start up all the background consumers / consumers required to run the workflow. Run
	// only needs to be called once. Any subsequent calls to run are safe and are noop.
	Run(ctx context.Context)

	// Stop tells the workflow to shut down gracefully.
	Stop()
}

type Workflow[Type any, Status StatusType] struct {
	Name string

	ctx    context.Context
	cancel context.CancelFunc

	clock                   clock.Clock
	defaultPollingFrequency time.Duration
	defaultErrBackOff       time.Duration
	defaultLagAlert         time.Duration

	calledRun bool
	once      sync.Once

	eventStreamerFn EventStreamer
	recordStore     RecordStore
	timeoutStore    TimeoutStore
	scheduler       RoleScheduler

	consumers map[Status][]consumerConfig[Type, Status]
	callback  map[Status][]callback[Type, Status]
	timeouts  map[Status]timeouts[Type, Status]

	workflowConnectorConfigs []workflowConnectorConfig[Type, Status]
	connectorConfigs         []connectorConfig[Type, Status]

	internalStateMu sync.Mutex
	// internalState holds the State of all expected consumers and timeout  go routines using their role names
	// as the key.
	internalState map[string]State

	graph      map[int][]int
	graphOrder []int

	endPoints     map[Status]bool
	validStatuses map[Status]bool

	debugMode bool
}

func (w *Workflow[Type, Status]) Run(ctx context.Context) {
	// Ensure that the background consumers are only initialized once
	w.once.Do(func() {
		ctx, cancel := context.WithCancel(ctx)
		w.ctx = ctx
		w.cancel = cancel
		w.calledRun = true

		for currentStatus, consumers := range w.consumers {
			for _, p := range consumers {
				if p.ParallelCount < 2 {
					// Launch all consumers in runners
					go consumer(w, currentStatus, p, 1, 1)
				} else {
					// Run as sharded parallel consumers
					for i := 1; i <= p.ParallelCount; i++ {
						go consumer(w, currentStatus, p, i, p.ParallelCount)
					}
				}
			}
		}

		for status, timeouts := range w.timeouts {
			go timeoutPoller(w, status, timeouts)
			go timeoutAutoInserterConsumer(w, status, timeouts)
		}

		for _, config := range w.workflowConnectorConfigs {
			if config.parallelCount < 2 {
				// Launch all consumers in runners
				go workflowConnectorConsumer(w, &config, 1, 1)
			} else {
				// Run as sharded parallel consumers
				for i := 1; i <= config.parallelCount; i++ {
					go workflowConnectorConsumer(w, &config, i, config.parallelCount)
				}
			}
		}

		for _, config := range w.connectorConfigs {
			if config.parallelCount < 2 {
				// Launch all consumers in runners
				go connectorConsumer(w, &config, 1, 1)
			} else {
				// Run as sharded parallel consumers
				for i := 1; i <= config.parallelCount; i++ {
					go connectorConsumer(w, &config, i, config.parallelCount)
				}
			}
		}
	})
}

// run is a standardise way of running blocking calls forever with retry such as consumers that need to adhere to role scheduling
func (w *Workflow[Type, Status]) run(role, processName string, process func(ctx context.Context) error, errBackOff time.Duration) {
	w.updateState(processName, StateIdle)
	defer w.updateState(processName, StateShutdown)

	for {
		if w.ctx.Err() != nil {
			// Parent context has been cancelled (likely from calling w.Stop) so return and don't attempt to assume a
			// role.
			if w.debugMode {
				log.Info(w.ctx, "shutting down", j.MKV{
					"role":         role,
					"process_name": processName,
				})
			}
			return
		}

		ctx, cancel, err := w.scheduler.Await(w.ctx, role)
		if errors.IsAny(err, context.Canceled) {
			// Exit cleanly if error returned is cancellation of context
			return
		} else if err != nil {
			log.Error(ctx, errors.Wrap(err, "error awaiting role", j.MKV{
				"role":         role,
				"process_name": processName,
			}))
			time.Sleep(errBackOff)
			continue
		}

		w.updateState(processName, StateRunning)

		err = process(ctx)
		if errors.IsAny(err, context.Canceled) {
			// Continue to re-evaluate parent context validity and if valid then attempt to assume the role again
			continue
		} else if err != nil {
			log.Error(ctx, errors.Wrap(err, "process error", j.MKV{
				"role": role,
			}))
			metrics.ProcessErrors.WithLabelValues(w.Name, processName).Inc()
		} else if err == nil {
			// If error is nil then the process finishes successfully, then release the role by cancelling the
			// context allowing other instances to obtain the role.
			cancel()
			continue
		}

		// Only in a non-nil error case
		select {
		case <-ctx.Done():
			cancel()
			continue
		case <-time.After(errBackOff):
			cancel()
		}
	}
}

// Stop cancels the context provided to all the background processes that the workflow launched and waits for all of
// them to shut down gracefully.
func (w *Workflow[Type, Status]) Stop() {
	if w.cancel == nil {
		return
	}

	// Cancel the parent context of the workflow to gracefully shutdown.
	w.cancel()

	for {
		var runningProcesses int
		for _, state := range w.States() {
			switch state {
			case StateUnknown, StateShutdown:
				continue
			default:
				runningProcesses++
			}
		}

		// Once all processes have exited then return
		if runningProcesses == 0 {
			return
		}
	}
}

func update(ctx context.Context, streamer EventStreamer, store RecordStore, wr *WireRecord) error {
	return store.Store(ctx, wr, func(id int64) error {
		// Update ID in-case the store is an append only store and the ID changes with every update
		wr.ID = id

		topic := Topic(wr.WorkflowName, wr.Status)

		headers := make(map[Header]string)
		headers[HeaderWorkflowForeignID] = wr.ForeignID
		headers[HeaderWorkflowName] = wr.WorkflowName
		headers[HeaderTopic] = topic
		headers[HeaderRunID] = wr.RunID

		producer := streamer.NewProducer(topic)
		err := producer.Send(ctx, wr.ID, wr.Status, headers)
		if err != nil {
			return err
		}

		return producer.Close()
	})
}
