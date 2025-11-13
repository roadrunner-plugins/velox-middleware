package veloxmiddleware

import (
	"sync"
	"sync/atomic"
)

// semaphore implements a counting semaphore for limiting concurrent operations.
type semaphore struct {
	slots   chan struct{}
	active  int32
	maxSize int
}

// newSemaphore creates a new semaphore with the specified maximum concurrent operations.
func newSemaphore(max int) *semaphore {
	return &semaphore{
		slots:   make(chan struct{}, max),
		maxSize: max,
	}
}

// acquire blocks until a slot is available or returns a channel that will signal when available.
func (s *semaphore) acquire() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		s.slots <- struct{}{}
		atomic.AddInt32(&s.active, 1)
		close(ch)
	}()
	return ch
}

// tryAcquire attempts to acquire a slot without blocking.
// Returns true if acquired, false if no slots available.
func (s *semaphore) tryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		atomic.AddInt32(&s.active, 1)
		return true
	default:
		return false
	}
}

// release releases a semaphore slot.
func (s *semaphore) release() {
	<-s.slots
	atomic.AddInt32(&s.active, -1)
}

// active returns the number of currently active operations.
func (s *semaphore) active() int {
	return int(atomic.LoadInt32(&s.active))
}

// bufferPool provides a pool of reusable buffers for streaming operations.
var bufferPool = sync.Pool{
	New: func() interface{} {
		// 32KB buffer size for efficient streaming
		buf := make([]byte, 32*1024)
		return buf
	},
}
