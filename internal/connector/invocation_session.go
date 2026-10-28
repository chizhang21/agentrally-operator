package connector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"
)

const (
	maxInvocationHistory = 1024
	maxInvocationQueue   = 64
	maxInvocationGates   = 64
)

type invocationRecord struct {
	command InvocationCommand
	hash    [32]byte
	ctx     context.Context
	cancel  context.CancelCauseFunc
}

type invocationGate struct {
	epoch         uint64
	action        string
	drainDeadline *time.Time
	drain         *time.Timer
}

type controlReceipt struct {
	hash   [32]byte
	result InvocationControlResult
}

// A session owns all dispatch authority. Nothing survives an epoch change.
// The lock orders Close/Cancel with dispatch; cancellation only stops forwarding.
type invocationSession struct {
	ctx     context.Context
	cancel  context.CancelFunc
	epoch   string
	resolve func(context.Context, InvocationIdentity) (invocationTarget, error)
	forward func(context.Context, InvocationCommand, func() error) InvocationResult
	ack     func(context.Context, InvocationResult) error

	mu                    sync.Mutex
	changed               chan struct{}
	records               map[string]*invocationRecord
	gates                 map[InvocationIdentity]*invocationGate
	cancels               map[string]InvocationControl
	controls              map[string]controlReceipt
	queued, starts, pings int
	groups                map[string]int
	instances             map[string]int
	pingInstances         map[string]int
	wg                    sync.WaitGroup
}

func newInvocationSession(ctx context.Context, epoch string) *invocationSession {
	ctx, cancel := context.WithCancel(ctx)
	return &invocationSession{ctx: ctx, cancel: cancel, epoch: epoch, changed: make(chan struct{}),
		records: map[string]*invocationRecord{}, gates: map[InvocationIdentity]*invocationGate{},
		cancels: map[string]InvocationControl{}, controls: map[string]controlReceipt{},
		groups: map[string]int{}, instances: map[string]int{}, pingInstances: map[string]int{}}
}

func (s *invocationSession) close() {
	s.cancel()
	s.mu.Lock()
	for _, gate := range s.gates {
		if gate.drain != nil {
			gate.drain.Stop()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *invocationSession) signal() { close(s.changed); s.changed = make(chan struct{}) }

func commandHash(value any) [32]byte { data, _ := json.Marshal(value); return sha256.Sum256(data) }

func (s *invocationSession) submit(command InvocationCommand) error {
	if err := validateInvocation(command, s.epoch, time.Now()); err != nil {
		return err
	}
	hash := commandHash(command)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return invocationError("SessionClosed")
	}
	if previous := s.records[command.RequestID]; previous != nil {
		if previous.hash != hash {
			return invocationError("RequestConflict")
		}
		return nil // The original worker owns result delivery, including retries.
	}
	if len(s.records) >= maxInvocationHistory {
		return invocationError("SessionHistoryFull")
	}
	if s.queued >= maxInvocationQueue {
		command.Body, command.Headers = nil, nil
		s.records[command.RequestID] = &invocationRecord{command: command, hash: hash, cancel: func(error) {}}
		return invocationError("InvocationQueueFull")
	}
	maximum := 300 * time.Second
	if command.Operation == "Ping" {
		maximum = 5 * time.Second
	}
	deadline := time.Now().Add(maximum)
	if command.ExpiresAt.Before(deadline) {
		deadline = command.ExpiresAt
	}
	ctx, deadlineCancel := context.WithDeadline(s.ctx, deadline)
	ctx, cancel := context.WithCancelCause(ctx)
	record := &invocationRecord{command: command, hash: hash, ctx: ctx, cancel: cancel}
	if tombstone, exists := s.cancels[command.RequestID]; exists {
		if !sameCancellation(tombstone, command) {
			deadlineCancel()
			cancel(nil)
			return invocationError("CancelConflict")
		}
		cancel(invocationError("Cancelled"))
	}
	s.records[command.RequestID] = record
	s.queued++
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer deadlineCancel()
		defer cancel(nil)
		s.run(record)
	}()
	return nil
}

func (s *invocationSession) run(record *invocationRecord) {
	acquired, err := s.acquire(record)
	result := InvocationResult{RequestID: record.command.RequestID, ExecutionID: record.command.ExecutionID, Delivery: "NotDispatched"}
	if err != nil {
		result.Reason = invocationReason(err)
	} else {
		result = s.forward(record.ctx, record.command, func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if record.ctx.Err() != nil {
				return cancellationReason(record.ctx)
			}
			if err := s.allowed(record.command); err != nil {
				return err
			}
			return nil
		})
	}
	// Release payload memory before any potentially slow result acknowledgement.
	s.mu.Lock()
	record.command.Body, record.command.Headers = nil, nil
	s.mu.Unlock()
	// The call deadline/cancellation does not cancel its result ACK. A lost ACK
	// closes the session, without issuing another application request.
	if s.ctx.Err() == nil {
		ackCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		if s.ack(ackCtx, result) != nil {
			s.cancel()
		}
		cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if acquired {
		s.budget(record.command, -1)
	} else {
		s.queued-- // Rejected calls retain a queue slot until their ACK completes.
	}
	record.command.Body, record.command.Headers = nil, nil
	s.signal()
}

func (s *invocationSession) acquire(record *invocationRecord) (bool, error) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if record.ctx.Err() != nil {
			return false, cancellationReason(record.ctx)
		}
		if err := s.allowed(record.command); err != nil {
			return false, err
		}
		if s.available(record.command) {
			s.budget(record.command, 1)
			s.queued--
			return true, nil
		}
		changed := s.changed
		s.mu.Unlock()
		var err error
		select {
		case <-record.ctx.Done():
			err = cancellationReason(record.ctx)
		case <-timer.C:
			err = invocationError("QueueDeadlineExceeded")
		case <-changed:
		}
		s.mu.Lock()
		if err != nil {
			return false, err
		}
	}
}

func (s *invocationSession) allowed(command InvocationCommand) error {
	if command.Operation == "Ping" {
		return nil
	}
	gate := s.gates[command.InvocationIdentity]
	if gate == nil || gate.action != "Open" || gate.epoch != command.GateEpoch {
		return invocationError("GateClosed")
	}
	return nil
}

func (s *invocationSession) available(command InvocationCommand) bool {
	if command.Operation == "Ping" {
		return s.pings < 2 && s.pingInstances[command.InstanceUID] < 2
	}
	return s.starts < 16 && s.groups[command.Scope.GroupUID] < 8 && s.instances[command.InstanceUID] < 1
}

func (s *invocationSession) budget(command InvocationCommand, delta int) {
	if command.Operation == "Ping" {
		s.pings += delta
		adjustBudget(s.pingInstances, command.InstanceUID, delta)
		return
	}
	s.starts += delta
	adjustBudget(s.groups, command.Scope.GroupUID, delta)
	adjustBudget(s.instances, command.InstanceUID, delta)
}

func adjustBudget(values map[string]int, key string, delta int) {
	values[key] += delta
	if values[key] == 0 {
		delete(values, key)
	}
}

func cancellationReason(ctx context.Context) error {
	if cause, ok := context.Cause(ctx).(invocationError); ok {
		return cause
	}
	if ctx.Err() == context.DeadlineExceeded {
		return invocationError("DeadlineExceeded")
	}
	return invocationError("Cancelled")
}

func sameCancellation(control InvocationControl, command InvocationCommand) bool {
	return control.InvocationIdentity == command.InvocationIdentity && control.IssuedAt.Equal(command.IssuedAt) && control.ExpiresAt.Equal(command.ExpiresAt)
}

func (s *invocationSession) control(ctx context.Context, control InvocationControl) InvocationControlResult {
	result := InvocationControlResult{ID: control.ID, Status: 409, Reason: "InvalidControl"}
	if err := validateControl(control, s.epoch); err != nil {
		result.Status = 400
		result.Reason = invocationReason(err)
		return result
	}
	hash := commandHash(control)
	// Open rechecks current identity. No lock is held across API-server IO.
	if control.Kind == "Gate" && control.Action == "Open" {
		if _, err := s.resolve(ctx, control.InvocationIdentity); err != nil {
			result.Reason = invocationReason(err)
			return result
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		result.Reason = "SessionClosed"
		return result
	}
	if prior, found := s.controls[control.ID]; found {
		if prior.hash == hash {
			return prior.result
		}
		result.Reason = "ControlConflict"
		return result
	}
	if len(s.controls) >= maxInvocationHistory {
		s.cancel()
		result.Reason = "SessionHistoryFull"
		return result
	}
	if control.Kind == "Cancel" {
		result = s.cancelCall(control)
	} else {
		result = s.updateGate(control)
	}
	s.controls[control.ID] = controlReceipt{hash: hash, result: result}
	s.signal()
	return result
}

func validateControl(control InvocationControl, epoch string) error {
	if control.ConnectionEpoch != epoch || !wireID.MatchString(control.ID) {
		return invocationError("InvalidSessionOrControl")
	}
	if err := validateInvocationIdentity(control.InvocationIdentity); err != nil {
		return err
	}
	if err := validateInvocationWindow(control.IssuedAt, control.ExpiresAt, time.Now(), 300*time.Second); err != nil {
		return err
	}
	switch control.Kind {
	case "Cancel":
		if !wireID.MatchString(control.RequestID) || control.Reason != "ClientCancelled" || control.Action != "" || control.GateEpoch != 0 {
			return invocationError("InvalidCancel")
		}
	case "Gate":
		if control.RequestID != "" || control.Reason != "" || control.GateEpoch == 0 || (control.Action != "Open" && control.Action != "Close" && control.Action != "Inspect") {
			return invocationError("InvalidGate")
		}
	default:
		return invocationError("UnsupportedControl")
	}
	return nil
}

func (s *invocationSession) cancelCall(control InvocationControl) InvocationControlResult {
	result := InvocationControlResult{ID: control.ID, Status: 200, Reason: "ForwardingCancelled"}
	if record := s.records[control.RequestID]; record != nil {
		if !sameCancellation(control, record.command) {
			result.Status = 409
			result.Reason = "CancelConflict"
			return result
		}
		record.cancel(invocationError("Cancelled"))
		return result
	}
	for id, cancel := range s.cancels {
		if !cancel.ExpiresAt.After(time.Now()) {
			delete(s.cancels, id)
		}
	}
	if old, exists := s.cancels[control.RequestID]; exists {
		// Control IDs may differ on retries; the cancellation identity may not.
		old.ID = control.ID
		if commandHash(old) != commandHash(control) {
			result.Status = 409
			result.Reason = "CancelConflict"
		}
		return result
	}
	if len(s.cancels) >= 64 {
		s.cancel()
		result.Status = 409
		result.Reason = "CancelHistoryFull"
		return result
	}
	s.cancels[control.RequestID] = control
	return result
}

func (s *invocationSession) updateGate(control InvocationControl) InvocationControlResult {
	result := InvocationControlResult{ID: control.ID, Status: 409, Reason: "GateConflict"}
	gate := s.gates[control.InvocationIdentity]
	if gate == nil {
		if control.Action == "Inspect" {
			result.Reason = "GateUnknown"
			return result
		}
		if len(s.gates) >= maxInvocationGates {
			s.cancel()
			result.Reason = "GateHistoryFull"
			return result
		}
		gate = &invocationGate{}
		s.gates[control.InvocationIdentity] = gate
	}
	if control.Action != "Inspect" {
		if control.GateEpoch < gate.epoch || control.GateEpoch == gate.epoch && control.Action != gate.action {
			return result
		}
		if control.GateEpoch > gate.epoch {
			if control.Action == "Open" && s.instances[control.InstanceUID] != 0 {
				result.Reason = "InvocationDraining"
				return result
			}
			gate.epoch, gate.action = control.GateEpoch, control.Action
			if gate.drain != nil {
				gate.drain.Stop()
			}
			gate.drainDeadline = nil
			if control.Action == "Close" {
				deadline := time.Now().UTC().Add(30 * time.Second)
				gate.drainDeadline = &deadline
				gate.drain = time.AfterFunc(30*time.Second, func() {
					s.mu.Lock()
					defer s.mu.Unlock()
					if gate.epoch != control.GateEpoch || gate.action != "Close" {
						return
					}
					for _, record := range s.records {
						if record.command.Operation == "Start" && record.command.InvocationIdentity == control.InvocationIdentity {
							record.cancel(invocationError("DrainDeadlineExceeded"))
						}
					}
				})
			}
		}
	}
	result.Status, result.Reason = 200, "GateObserved"
	result.GateEpoch, result.DrainDeadline = gate.epoch, gate.drainDeadline
	result.ActiveStarts = s.instances[control.InstanceUID]
	return result
}
