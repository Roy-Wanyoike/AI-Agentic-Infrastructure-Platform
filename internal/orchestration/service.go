package orchestration

// service.go — issue #108: the orchestration runtime facade.
//
// Service bundles the three pieces of the agent-to-agent orchestration
// executor — the in-process communication bus, the delegation protocol
// service and the task supervisor — behind one constructor for the API
// wiring layer (cmd/api/orchestration.go). Everything is in-memory BY DESIGN
// this round (no bus/ledger migration is assigned): a NATS-backed
// AgentCommBus can slot into the delegation service later without touching
// callers, and the delegation ledger can gain a durable store the same way.

import "time"

// Options configures the facade. Zero values select the documented defaults
// of each component.
type Options struct {
	// Seams (all required for delegation to actually run; the executor may
	// be attached later via SetExecutor when the wiring layer builds it
	// after the rest of the service graph).
	Runs      RunFactory
	Executor  Executor
	Approvals ApprovalRouter

	// Supervisor routing table (deployment configuration for this process).
	Workers  []WorkerAgent
	Strategy RoutingStrategy // default round_robin

	// Supervisor caps (<= 0 selects each default).
	SupervisorMaxConcurrent int
	SupervisorMaxRetries    int

	// Delegation + bus tuning (<= 0 selects each default).
	MaxDepth           int
	DelegationTimeout  time.Duration
	BusChannelCapacity int
	BusInboxRing       int
}

// Service is the orchestration runtime facade: bus + delegation protocol +
// supervisor. Safe for concurrent use.
type Service struct {
	Bus        *InProcessCommBus
	Delegation *DelegationService
	Supervisor *Supervisor
}

// NewService builds the facade with the given options.
func NewService(opts Options) (*Service, error) {
	if opts.SupervisorMaxConcurrent <= 0 {
		opts.SupervisorMaxConcurrent = DefaultSupervisorConcurrency
	}
	if opts.SupervisorMaxRetries <= 0 {
		opts.SupervisorMaxRetries = DefaultSupervisorRetries
	}
	bus := NewInProcessCommBus(BusOptions{
		ChannelCapacity: opts.BusChannelCapacity,
		InboxRing:       opts.BusInboxRing,
	})
	delegation := NewDelegationService(DelegationOptions{
		Bus:       bus,
		Runs:      opts.Runs,
		Executor:  opts.Executor,
		Approvals: opts.Approvals,
		MaxDepth:  opts.MaxDepth,
		Timeout:   opts.DelegationTimeout,
	})
	supervisor, err := NewSupervisor(SupervisorConfig{
		Workers:       opts.Workers,
		Strategy:      opts.Strategy,
		MaxConcurrent: opts.SupervisorMaxConcurrent,
		MaxRetries:    opts.SupervisorMaxRetries,
	}, delegation)
	if err != nil {
		return nil, err
	}
	return &Service{Bus: bus, Delegation: delegation, Supervisor: supervisor}, nil
}

// SetExecutor attaches (or replaces) the runtime execution seam. Nil-safe on
// the receiver. Used by the wiring layer when the executor is constructed
// after the rest of the service graph (the shared runtime runner needs the
// provider resolved from the environment first).
func (s *Service) SetExecutor(ex Executor) {
	if s == nil {
		return
	}
	s.Delegation.SetExecutor(ex)
}
