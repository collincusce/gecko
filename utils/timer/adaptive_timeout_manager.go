// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package timer

import (
	"container/heap"
	"sync"
	"time"

	"github.com/ava-labs/gecko/ids"
	"github.com/prometheus/client_golang/prometheus"
)

type adaptiveTimeout struct {
	index    int           // Index in the wait queue
	id       ids.ID        // Unique ID of this timeout
	handler  func()        // Function to execute if timed out
	duration time.Duration // How long this timeout was set for
	deadline time.Time     // When this timeout should be fired
}

// A timeoutQueue implements heap.Interface and holds adaptiveTimeouts.
type timeoutQueue []*adaptiveTimeout

func (tq timeoutQueue) Len() int           { return len(tq) }
func (tq timeoutQueue) Less(i, j int) bool { return tq[i].deadline.Before(tq[j].deadline) }
func (tq timeoutQueue) Swap(i, j int) {
	tq[i], tq[j] = tq[j], tq[i]
	tq[i].index = i
	tq[j].index = j
}

// Push adds an item to this priority queue. x must have type *adaptiveTimeout
func (tq *timeoutQueue) Push(x interface{}) {
	item := x.(*adaptiveTimeout)
	item.index = len(*tq)
	*tq = append(*tq, item)
}

// Pop returns the next item in this queue
func (tq *timeoutQueue) Pop() interface{} {
	n := len(*tq)
	item := (*tq)[n-1]
	(*tq)[n-1] = nil // make sure the item is freed from memory
	*tq = (*tq)[:n-1]
	return item
}

// AdaptiveTimeoutManager is a manager for timeouts.
type AdaptiveTimeoutManager struct {
	currentDurationMetric prometheus.Gauge

	minimumDuration time.Duration
	increaseRatio   float64
	decreaseValue   time.Duration

	lock            sync.Mutex
	currentDuration time.Duration // Amount of time before a timeout
	timeoutMap      map[[32]byte]*adaptiveTimeout
	timeoutQueue    timeoutQueue
	timer           *Timer // Timer that will fire to clear the timeouts
}

// Initialize is a constructor b/c Golang, in its wisdom, doesn't ... have them?
func (tm *AdaptiveTimeoutManager) Initialize(
	initialDuration time.Duration,
	minimumDuration time.Duration,
	increaseRatio float64,
	decreaseValue time.Duration,
	namespace string,
	registerer prometheus.Registerer,
) error {
	tm.currentDurationMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "network_timeout",
		Help:      "Duration of current network timeouts in nanoseconds",
	})
	tm.minimumDuration = minimumDuration
	tm.increaseRatio = increaseRatio
	tm.decreaseValue = decreaseValue
	tm.currentDuration = initialDuration
	tm.timeoutMap = make(map[[32]byte]*adaptiveTimeout)
	tm.timer = NewTimer(tm.Timeout)
	return registerer.Register(tm.currentDurationMetric)
}

// Dispatch ...
func (tm *AdaptiveTimeoutManager) Dispatch() { tm.timer.Dispatch() }

// Stop executing timeouts
func (tm *AdaptiveTimeoutManager) Stop() { tm.timer.Stop() }

// Put puts hash into the hash map
func (tm *AdaptiveTimeoutManager) Put(id ids.ID, handler func()) time.Time {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	return tm.put(id, handler)
}

// Remove the item that no longer needs to be there.
func (tm *AdaptiveTimeoutManager) Remove(id ids.ID) {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	currentTime := time.Now()

	tm.remove(id, currentTime)
}

// Timeout registers a timeout
func (tm *AdaptiveTimeoutManager) Timeout() {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	tm.timeout()
}

func (tm *AdaptiveTimeoutManager) timeout() {
	currentTime := time.Now()
	// removeExpiredHead returns nil once there is nothing left to remove
	for {
		timeout := tm.removeExpiredHead(currentTime)
		if timeout == nil {
			break
		}

		// Don't execute a callback with a lock held
		tm.lock.Unlock()
		timeout()
		tm.lock.Lock()
	}
	tm.registerTimeout()
}

func (tm *AdaptiveTimeoutManager) put(id ids.ID, handler func()) time.Time {
	currentTime := time.Now()
	tm.remove(id, currentTime)

	timeout := &adaptiveTimeout{
		id:       id,
		handler:  handler,
		duration: tm.currentDuration,
		deadline: currentTime.Add(tm.currentDuration),
	}
	tm.timeoutMap[id.Key()] = timeout
	heap.Push(&tm.timeoutQueue, timeout)

	tm.registerTimeout()
	return timeout.deadline
}

func (tm *AdaptiveTimeoutManager) remove(id ids.ID, currentTime time.Time) {
	key := id.Key()
	timeout, exists := tm.timeoutMap[key]
	if !exists {
		return
	}

	if timeout.deadline.Before(currentTime) {
		// This request is being removed because it timed out.
		if timeout.duration >= tm.currentDuration {
			// If the current timeout duration is less than or equal to the
			// timeout that was triggered, double the duration.
			tm.currentDuration = time.Duration(float64(tm.currentDuration) * tm.increaseRatio)
		}
	} else {
		// This request is being removed because it finished successfully.
		if timeout.duration <= tm.currentDuration {
			// If the current timeout duration is greater than or equal to the
			// timeout that was fullfilled, reduce future timeouts.
			tm.currentDuration -= tm.decreaseValue

			if tm.currentDuration < tm.minimumDuration {
				// Make sure that we never get stuck in a bad situation
				tm.currentDuration = tm.minimumDuration
			}
		}
	}

	// Make sure the metrics report the current timeouts
	tm.currentDurationMetric.Set(float64(tm.currentDuration))

	// Remove the timeout from the map
	delete(tm.timeoutMap, key)

	// Remove the timeout from the queue
	heap.Remove(&tm.timeoutQueue, timeout.index)
}

// Returns true if the head was removed, false otherwise
func (tm *AdaptiveTimeoutManager) removeExpiredHead(currentTime time.Time) func() {
	if tm.timeoutQueue.Len() == 0 {
		return nil
	}

	nextTimeout := tm.timeoutQueue[0]
	if nextTimeout.deadline.After(currentTime) {
		return nil
	}
	tm.remove(nextTimeout.id, currentTime)
	return nextTimeout.handler
}

func (tm *AdaptiveTimeoutManager) registerTimeout() {
	if tm.timeoutQueue.Len() == 0 {
		// There are no pending timeouts
		tm.timer.Cancel()
		return
	}

	currentTime := time.Now()
	nextTimeout := tm.timeoutQueue[0]
	timeToNextTimeout := nextTimeout.deadline.Sub(currentTime)
	tm.timer.SetTimeoutIn(timeToNextTimeout)
}
