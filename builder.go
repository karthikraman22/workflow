package workflow

import (
	"context"
	"os"
	"time"

	"k8s.io/utils/clock"

	"github.com/luno/workflow/internal/errorcounter"
	"github.com/luno/workflow/internal/graph"
	interal_logger "github.com/luno/workflow/internal/logger"
)

const (
	defaultPollingFrequency = 500 * time.Millisecond
	defaultErrBackOff       = 1 * time.Second
	defaultLagAlert         = 30 * time.Minute

	defaultOutboxLagAlert         = time.Minute
	defaultOutboxPollingFrequency = 250 * time.Millisecond
	defaultOutboxErrBackOff       = 500 * time.Millisecond
)

func NewBuilder[Type any, Status StatusType](name string) *Builder[Type, Status] {
	return &Builder[Type, Status]{
		workflow: &Workflow[Type, Status]{
			name:          name,
			clock:         clock.RealClock{},
			consumers:     make(map[Status]consumerConfig[Type, Status]),
			callback:      make(map[Status][]callback[Type, Status]),
			timeouts:      make(map[Status]timeouts[Type, Status]),
			statusGraph:   graph.New(),
			errorCounter:  errorcounter.New(),
			internalState: make(map[string]State),
			logger: &logger{
				debugMode: false, // Explicit for readability
				inner:     interal_logger.New(os.Stdout),
			},
			runStateChangeHooks: make(map[RunState]RunStateChangeHookFunc[Type, Status]),
		},
	}
}

type Builder[Type any, Status StatusType] struct {
	workflow *Workflow[Type, Status]
}

func (b *Builder[Type, Status]) AddStep(
	from Status,
	c ConsumerFunc[Type, Status],
	allowedDestinations ...Status,
) *stepUpdater[Type, Status] {
	if _, exists := b.workflow.consumers[from]; exists {
		panic("'AddStep(" + from.String() + ",' already exists. Only one Step can be configured to consume the status")
	}

	for _, to := range allowedDestinations {
		b.workflow.statusGraph.AddTransition(int(from), int(to))
	}

	p := consumerConfig[Type, Status]{
		consumer: c,
	}

	b.workflow.consumers[from] = p

	return &stepUpdater[Type, Status]{
		from:     from,
		workflow: b.workflow,
	}
}

type stepUpdater[Type any, Status StatusType] struct {
	from     Status
	workflow *Workflow[Type, Status]
}

func (s *stepUpdater[Type, Status]) WithOptions(opts ...Option) {
	consumer := s.workflow.consumers[s.from]

	var consumerOpts options
	for _, opt := range opts {
		opt(&consumerOpts)
	}

	consumer.pollingFrequency = consumerOpts.pollingFrequency
	consumer.parallelCount = consumerOpts.parallelCount
	consumer.errBackOff = consumerOpts.errBackOff
	consumer.lag = consumerOpts.lag
	consumer.lagAlert = consumerOpts.lagAlert
	consumer.pauseAfterErrCount = consumerOpts.pauseAfterErrCount
	s.workflow.consumers[s.from] = consumer
}

func (b *Builder[Type, Status]) AddCallback(from Status, fn CallbackFunc[Type, Status], allowedDestinations ...Status) {
	c := callback[Type, Status]{
		CallbackFunc: fn,
	}

	for _, to := range allowedDestinations {
		b.workflow.statusGraph.AddTransition(int(from), int(to))
	}

	b.workflow.callback[from] = append(b.workflow.callback[from], c)
}

func (b *Builder[Type, Status]) AddTimeout(
	from Status,
	timer TimerFunc[Type, Status],
	tf TimeoutFunc[Type, Status],
	allowedDestinations ...Status,
) *timeoutUpdater[Type, Status] {
	timeouts := b.workflow.timeouts[from]

	t := timeout[Type, Status]{
		TimerFunc:   timer,
		TimeoutFunc: tf,
	}

	for _, to := range allowedDestinations {
		b.workflow.statusGraph.AddTransition(int(from), int(to))
	}

	timeouts.transitions = append(timeouts.transitions, t)
	b.workflow.timeouts[from] = timeouts

	return &timeoutUpdater[Type, Status]{
		from:     from,
		workflow: b.workflow,
	}
}

type timeoutUpdater[Type any, Status StatusType] struct {
	from     Status
	workflow *Workflow[Type, Status]
}

func (s *timeoutUpdater[Type, Status]) WithOptions(opts ...Option) {
	timeout := s.workflow.timeouts[s.from]

	var timeoutOpts options
	for _, opt := range opts {
		opt(&timeoutOpts)
	}

	if timeoutOpts.parallelCount != 0 {
		panic("Cannot configure parallel timeout")
	}

	if timeoutOpts.lag != 0 {
		panic("Cannot configure lag for timeout")
	}

	timeout.pollingFrequency = timeoutOpts.pollingFrequency
	timeout.errBackOff = timeoutOpts.errBackOff
	timeout.lagAlert = timeoutOpts.lagAlert
	timeout.pauseAfterErrCount = timeoutOpts.pauseAfterErrCount
	s.workflow.timeouts[s.from] = timeout
}

func (b *Builder[Type, Status]) AddConnector(
	name string,
	csc ConnectorConstructor,
	cf ConnectorFunc[Type, Status],
) *connectorUpdater[Type, Status] {
	for _, config := range b.workflow.connectorConfigs {
		if config.name == name {
			panic("connector names need to be unique")
		}
	}

	config := &connectorConfig[Type, Status]{
		name:        name,
		constructor: csc,
		connectorFn: cf,
	}

	b.workflow.connectorConfigs = append(b.workflow.connectorConfigs, config)
	return &connectorUpdater[Type, Status]{
		workflow: b.workflow,
		config:   config,
	}
}

type connectorUpdater[Type any, Status StatusType] struct {
	workflow *Workflow[Type, Status]
	config   *connectorConfig[Type, Status]
}

func (c *connectorUpdater[Type, Status]) WithOptions(opts ...Option) {
	var connectorOpts options
	for _, opt := range opts {
		opt(&connectorOpts)
	}

	c.config.parallelCount = connectorOpts.parallelCount
	c.config.errBackOff = connectorOpts.errBackOff
	c.config.lag = connectorOpts.lag
	c.config.lagAlert = connectorOpts.lagAlert
}

func (b *Builder[Type, Status]) OnPause(hook RunStateChangeHookFunc[Type, Status]) {
	b.workflow.runStateChangeHooks[RunStatePaused] = hook
}

func (b *Builder[Type, Status]) OnCancel(hook RunStateChangeHookFunc[Type, Status]) {
	b.workflow.runStateChangeHooks[RunStateCancelled] = hook
}

func (b *Builder[Type, Status]) OnComplete(hook RunStateChangeHookFunc[Type, Status]) {
	b.workflow.runStateChangeHooks[RunStateCompleted] = hook
}

func (b *Builder[Type, Status]) Build(
	eventStreamer EventStreamer,
	recordStore RecordStore,
	roleScheduler RoleScheduler,
	opts ...BuildOption,
) *Workflow[Type, Status] {
	b.workflow.eventStreamer = eventStreamer
	b.workflow.recordStore = recordStore
	b.workflow.scheduler = roleScheduler

	bo := defaultBuildOptions()
	for _, opt := range opts {
		opt(&bo)
	}

	if bo.clock != nil {
		b.workflow.clock = bo.clock
	}

	if bo.customDelete != nil {
		b.workflow.customDelete = bo.customDelete
	}

	b.workflow.timeoutStore = bo.timeoutStore
	b.workflow.defaultOpts = bo.defaultOptions
	b.workflow.outboxConfig = bo.outboxConfig
	b.workflow.logger.debugMode = bo.debugMode
	b.workflow.pausedRecordsRetry = bo.autoPauseRetry

	if bo.logger != nil {
		b.workflow.logger.inner = bo.logger
	}

	if len(b.workflow.timeouts) > 0 && b.workflow.timeoutStore == nil {
		panic("cannot configure timeouts without providing TimeoutStore for workflow")
	}

	return b.workflow
}

type buildOptions struct {
	clock          clock.Clock
	customDelete   customDelete
	debugMode      bool
	defaultOptions options
	outboxConfig   outboxConfig
	timeoutStore   TimeoutStore
	logger         Logger
	autoPauseRetry pausedRecordsRetry
}

func defaultBuildOptions() buildOptions {
	return buildOptions{
		outboxConfig:   defaultOutboxConfig(),
		defaultOptions: defaultOptions(),
		autoPauseRetry: defaultPausedRecordsRetry(),
	}
}

type BuildOption func(w *buildOptions)

// WithTimeoutStore allows the configuration of a TimeoutStore which is required when using timeouts in a workflow. It is
// not required by default as timeouts are less common of a feature requirement but when needed the abstraction
// of complexity of handling scheduling, expiring, and executing are incredibly useful and is included as one of the
// three key feature offerings of workflow which are sequential steps, callbacks, and timeouts.
func WithTimeoutStore(s TimeoutStore) BuildOption {
	return func(w *buildOptions) {
		w.timeoutStore = s
	}
}

// WithClock allows the configuring of workflow's use and access of time. Instead of using time.Now() and other
// associated functionality from the time package a clock is used instead in order to make it testable.
func WithClock(c clock.Clock) BuildOption {
	return func(bo *buildOptions) {
		bo.clock = c
	}
}

// WithDebugMode enabled debug mode for a workflow which results in increased logs such as when processes ar launched,
// shutdown, events are skipped etc.
func WithDebugMode() BuildOption {
	return func(bo *buildOptions) {
		bo.debugMode = true
	}
}

// WithLogger allows for specifying a custom logger. The default is to use a wrapped version of log/slog's Logger.
func WithLogger(l Logger) BuildOption {
	return func(bo *buildOptions) {
		bo.logger = l
	}
}

// WithDefaultOptions applies the provided options to the entire workflow and not just to an individual process.
func WithDefaultOptions(opts ...Option) BuildOption {
	return func(bo *buildOptions) {
		var o options
		for _, opt := range opts {
			opt(&o)
		}

		bo.defaultOptions = o
	}
}

// WithCustomDelete allows for specifying a custom deleter function for scrubbing PII data when a workflow Run enters
// RunStateRequestedDataDeleted and is the function that once executed successfully allows for the RunState to move to
// RunStateDataDeleted.
func WithCustomDelete[Type any](fn func(object *Type) error) BuildOption {
	return func(bo *buildOptions) {
		bo.customDelete = func(wr *Record) ([]byte, error) {
			var t Type
			err := Unmarshal(wr.Object, &t)
			if err != nil {
				return nil, err
			}

			err = fn(&t)
			if err != nil {
				return nil, err
			}

			return Marshal(&t)
		}
	}
}

// WithPauseRetry sets custom retry parameters for all paused records. The default is set to retry records that
// have been paused for an hour and will process in batches of 10 records at a time as to slowly introduce consumption.
//
// Parameters:
// - resumeAfter refers to the time that must elapse before a paused record is included in a cycle.
func WithPauseRetry(resumeAfter time.Duration) BuildOption {
	return func(bo *buildOptions) {
		bo.autoPauseRetry.enabled = true
		bo.autoPauseRetry.resumeAfter = resumeAfter
	}
}

// DisablePauseRetry sets disables the automatic retries of paused records. Paused records will result in no new
// workflow runs being able to be triggered for the provided foreign ID.
func DisablePauseRetry() BuildOption {
	return func(bo *buildOptions) {
		bo.autoPauseRetry.enabled = false
	}
}

func DurationTimerFunc[Type any, Status StatusType](duration time.Duration) TimerFunc[Type, Status] {
	return func(ctx context.Context, r *Run[Type, Status], now time.Time) (time.Time, error) {
		return now.Add(duration), nil
	}
}

func TimeTimerFunc[Type any, Status StatusType](t time.Time) TimerFunc[Type, Status] {
	return func(ctx context.Context, r *Run[Type, Status], now time.Time) (time.Time, error) {
		return t, nil
	}
}
