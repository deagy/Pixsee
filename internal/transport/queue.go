package transport

import (
	"errors"
	"sync"
)

var ErrBackpressure = errors.New("transport queue is full")

type Queue[T any] struct {
	mu     sync.Mutex
	values chan T
	closed bool
}

func NewQueue[T any](capacity int) (*Queue[T], error) {
	if capacity <= 0 {
		return nil, errors.New("queue capacity must be positive")
	}
	return &Queue[T]{values: make(chan T, capacity)}, nil
}

func (q *Queue[T]) TrySend(value T) error {
	if q == nil {
		return ErrClosed
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}
	select {
	case q.values <- value:
		return nil
	default:
		return ErrBackpressure
	}
}

func (q *Queue[T]) Receive() <-chan T {
	if q == nil {
		closed := make(chan T)
		close(closed)
		return closed
	}
	return q.values
}

func (q *Queue[T]) Close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.values)
	}
}
