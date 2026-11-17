package gosnowflake

import (
	"bytes"
	"runtime"
	"strconv"
	"sync"
)

type valueAwaiterType struct {
	lockKey lockKeyType
	working bool
	cond    *sync.Cond
	mu      sync.Mutex
	h       *valueAwaitHolderType
}

func newValueAwaiter(lockKey lockKeyType, h *valueAwaitHolderType) *valueAwaiterType {
	ret := &valueAwaiterType{
		lockKey: lockKey,
		h:       h,
	}
	ret.cond = sync.NewCond(&ret.mu)
	return ret
}

func awaitValue[T any](valueAwaiter *valueAwaiterType, runFunc func() (T, error), acceptFunc func(t T, err error) bool, defaultFactoryFunc func() T) (T, error) {
	logger.Tracef("awaitValue[%v] entered awaitValue for %s", goroutineID(), valueAwaiter.lockKey.lockID())
	valueAwaiter.mu.Lock()
	value, err := runFunc()

	// check if the value is already ready
	if acceptFunc(value, err) {
		logger.Tracef("awaitValue[%v] value was ready", goroutineID())
		valueAwaiter.mu.Unlock()
		return value, err
	}

	// value is not ready, check if no other thread is working
	if !valueAwaiter.working {
		logger.Tracef("awaitValue[%v] start working", goroutineID())
		valueAwaiter.working = true
		valueAwaiter.mu.Unlock()
		// continue working only in this thread
		return defaultFactoryFunc(), nil
	}

	// Check again if the value is ready after each wakeup.
	// If one thread is woken up and the value is still not ready, it should return default and continue working on this.
	// If the value is ready, all threads should be woken up and return the value.
	ret, err := runFunc()
	for !acceptFunc(ret, err) {
		logger.Tracef("awaitValue[%v] waiting for value", goroutineID())
		valueAwaiter.cond.Wait()
		logger.Tracef("awaitValue[%v] woke up", goroutineID())
		ret, err = runFunc()
		if !acceptFunc(ret, err) && !valueAwaiter.working {
			logger.Tracef("awaitValue[%v] start working after wait", goroutineID())
			valueAwaiter.working = true
			valueAwaiter.mu.Unlock()
			return defaultFactoryFunc(), nil
		}
	}

	// Value is ready - all threads should return the value.
	logger.Tracef("awaitValue[%v] value was ready after wait", goroutineID())
	valueAwaiter.mu.Unlock()
	return ret, err
}

func (v *valueAwaiterType) done() {
	logger.Tracef("valueAwaiter[%v] done working for %s, resuming all threads", goroutineID(), v.lockKey.lockID())
	v.mu.Lock()
	defer v.mu.Unlock()
	v.working = false
	v.cond.Broadcast()
	v.h.remove(v)
}

func (v *valueAwaiterType) resumeOne() {
	logger.Tracef("valueAwaiter[%v] done working for %s, resuming one thread", goroutineID(), v.lockKey.lockID())
	v.mu.Lock()
	defer v.mu.Unlock()
	v.working = false
	v.cond.Signal()
}

type valueAwaitHolderType struct {
	mu      sync.Mutex
	holders map[string]*valueAwaiterType
}

var valueAwaitHolder = newValueAwaitHolder()

func newValueAwaitHolder() *valueAwaitHolderType {
	return &valueAwaitHolderType{
		holders: make(map[string]*valueAwaiterType),
	}
}

func (h *valueAwaitHolderType) get(lockKey lockKeyType) *valueAwaiterType {
	lockID := lockKey.lockID()
	h.mu.Lock()
	defer h.mu.Unlock()
	holder, ok := h.holders[lockID]
	if !ok {
		holder = newValueAwaiter(lockKey, h)
		h.holders[lockID] = holder
	}
	return holder
}

func (h *valueAwaitHolderType) remove(v *valueAwaiterType) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.holders, v.lockKey.lockID())
}

func goroutineID() int {
	buf := make([]byte, 32)
	n := runtime.Stack(buf, false)
	buf = buf[:n]
	// goroutine 1 [running]: ...

	buf, ok := bytes.CutPrefix(buf, []byte("goroutine "))
	if !ok {
		return -1
	}

	i := bytes.IndexByte(buf, ' ')
	if i < 0 {
		return -2
	}

	goid, err := strconv.Atoi(string(buf[:i]))
	if err != nil {
		logger.Tracef("goroutineID err: %v", err)
		return -3
	}
	return goid
}
