package p2p

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

type ResponseTracker struct {
	mu      sync.Mutex
	pending map[common.Hash]bool
}

func NewResponseTracker() *ResponseTracker {
	return &ResponseTracker{
		pending: make(map[common.Hash]bool),
	}
}

// Register a new outstanding request
func (t *ResponseTracker) addRequest(hash common.Hash) {
	t.mu.Lock()
	t.pending[hash] = true
	t.mu.Unlock()
}

func (t *ResponseTracker) has(hash common.Hash) bool {
	t.mu.Lock()
	_, ok := t.pending[hash]
	t.mu.Unlock()
	return ok
}

// Called by OnUnsafeL2Response handler: if there's a waiting request,
// push the envelope into the channel and clean up.
func (t *ResponseTracker) remove(hash common.Hash) {
	t.mu.Lock()
	_, ok := t.pending[hash]
	if ok {
		delete(t.pending, hash)
	}

	t.mu.Unlock()
}
