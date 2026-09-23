package main

import (
	"context"
	"sync"
)

// inFlight tracks cancellable executor work so plugin.quiesce can drain it.
var inFlight = struct {
	mu      sync.Mutex
	next    uint64
	cancels map[uint64]context.CancelFunc
}{cancels: make(map[uint64]context.CancelFunc)}

func registerInFlight(cancel context.CancelFunc) uint64 {
	if cancel == nil {
		return 0
	}
	inFlight.mu.Lock()
	defer inFlight.mu.Unlock()
	inFlight.next++
	id := inFlight.next
	inFlight.cancels[id] = cancel
	return id
}

func unregisterInFlight(id uint64) {
	if id == 0 {
		return
	}
	inFlight.mu.Lock()
	delete(inFlight.cancels, id)
	inFlight.mu.Unlock()
}

func quiesce() {
	inFlight.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(inFlight.cancels))
	for _, cancel := range inFlight.cancels {
		cancels = append(cancels, cancel)
	}
	inFlight.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
