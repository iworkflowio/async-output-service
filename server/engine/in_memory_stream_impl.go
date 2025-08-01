package engine

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// StreamEntry represents a single output entry in the stream
type StreamEntry struct {
	OutputUUID uuid.UUID
	Output     OutputType
	Timestamp  time.Time
}

type InMemoryStreamImpl struct {
	outputs chan StreamEntry
	// indicates if the stream is stopped
	stopped bool
	// channel capacity for reference
	capacity int
	// channel to signal stop
	stopCh chan struct{}
	// protect the channel and state
	sync.RWMutex
}

var ErrStreamStopped = errors.New("stream is stopped")

var circularBufferMaxIterations = 100

func SetCircularBufferMaxIterations(maxIterations int) {
	circularBufferMaxIterations = maxIterations
}

func NewInMemoryStreamImpl(size int) InMemoeryStream {
	return &InMemoryStreamImpl{
		outputs:  make(chan StreamEntry, size),
		capacity: size,
		stopped:  false,
		stopCh:   make(chan struct{}),
	}
}

// Send implements InMemoeryStream.
func (i *InMemoryStreamImpl) Send(output OutputType, outputUuid uuid.UUID, timestamp time.Time, blockingWriteTimeoutSeconds int) (errorType ErrorType, err error) {
	// Check if stopped first
	if i.stopped {
		return ErrorTypeStreamStopped, ErrStreamStopped
	}

	entry := StreamEntry{
		OutputUUID: outputUuid,
		Output:     output,
		Timestamp:  timestamp,
	}

	// If blockingWriteTimeoutSeconds is 0 or not specified, use circular buffer mode
	if blockingWriteTimeoutSeconds <= 0 {
		return i.sendCircularBufferWithChannel(entry, i.outputs)
	}

	// Use blocking queue mode with timeout
	return i.sendBlockingQueueWithChannel(entry, blockingWriteTimeoutSeconds, i.outputs)
}

// sendCircularBufferWithChannel implements circular buffer behavior - overwrites oldest data when full
func (i *InMemoryStreamImpl) sendCircularBufferWithChannel(entry StreamEntry, outputsChan chan StreamEntry) (errorType ErrorType, err error) {
	// Not allowed for zero capacity circular buffer
	if i.capacity == 0 {
		return ErrorTypeInvalidRequest, errors.New("zero capacity circular buffer is not allowed")
	}

	select {
	case outputsChan <- entry:
		// Successfully wrote to channel
		return ErrorTypeNone, nil
	case <-i.stopCh:
		return ErrorTypeStreamStopped, ErrStreamStopped
	default:
		// Channel is full, remove oldest entry and add new one
		// Use write lock to protect the two operations below
		i.Lock()
		defer i.Unlock()

		// Check if stopped while waiting for lock
		if i.stopped {
			return ErrorTypeStreamStopped, ErrStreamStopped
		}

		iterations := 0
		for {
			iterations++
			if iterations > circularBufferMaxIterations {
				return ErrorTypeCircularBufferIterationLimit, fmt.Errorf("failed to write to circular buffer, buffer is still full after removing oldest entry for %d iterations", iterations)
			}
			// However, this is best effort only because other operations are not using locks.
			<-outputsChan // Remove oldest
			select {
			case outputsChan <- entry:
				// Successfully wrote to channel
				return ErrorTypeNone, nil
			case <-i.stopCh:
				return ErrorTypeStreamStopped, ErrStreamStopped
			default:
				// Channel is still full, do it again
				continue
			}
		}
	}
}

// sendBlockingQueueWithChannel implements blocking queue behavior - waits for space and returns error on timeout
func (i *InMemoryStreamImpl) sendBlockingQueueWithChannel(entry StreamEntry, timeoutSeconds int, outputsChan chan StreamEntry) (errorType ErrorType, err error) {
	select {
	case outputsChan <- entry:
		// Successfully wrote to channel
		return ErrorTypeNone, nil
	case <-i.stopCh:
		return ErrorTypeStreamStopped, ErrStreamStopped
	case <-time.After(time.Duration(timeoutSeconds) * time.Second):
		// NOTE: As of Go 1.23, the garbage collector can recover unreferenced unstopped timers. There is no reason to prefer NewTimer when After will do.
		return ErrorTypeWaitingTimeout, errors.New("timeout waiting for stream space (424)")
	}
}

// Receive implements InMemoeryStream.
func (i *InMemoryStreamImpl) Receive(timeoutSeconds int) (output *InternalReceiveResponse, errorType ErrorType, err error) {
	// Quick check if stopped (without lock since it's just a read)
	if i.stopped {
		return nil, ErrorTypeStreamStopped, ErrStreamStopped
	}

	select {
	case entry := <-i.outputs:
		// Successfully received an entry
		return &InternalReceiveResponse{
			OutputUuid: entry.OutputUUID,
			Output:     entry.Output,
			Timestamp:  entry.Timestamp,
		}, ErrorTypeNone, nil
	case <-i.stopCh:
		return nil, ErrorTypeStreamStopped, ErrStreamStopped
	case <-time.After(time.Duration(timeoutSeconds) * time.Second):
		// NOTE: As of Go 1.23, the garbage collector can recover unreferenced unstopped timers. There is no reason to prefer NewTimer when After will do.
		return nil, ErrorTypeWaitingTimeout, nil
	}
}

// Stop implements InMemoeryStream.
func (i *InMemoryStreamImpl) Stop() error {
	i.Lock()
	defer i.Unlock()

	if i.stopped {
		return nil
	}

	i.stopped = true
	close(i.stopCh) 
	// TODO move the received outputs to the new node that owned the streamId
	close(i.outputs)
	return nil
}
