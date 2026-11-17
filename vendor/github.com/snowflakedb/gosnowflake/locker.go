package gosnowflake

import "sync"

// ---------- API ----------
type lockKeyType interface {
	lockID() string
}

type locker interface {
	lock(lockKey lockKeyType) unlocker
}

type unlocker interface {
	Unlock()
}

func getValueWithLock[T any](locker locker, lockKey lockKeyType, f func() (T, error)) (T, error) {
	unlock := locker.lock(lockKey)
	defer unlock.Unlock()
	return f()
}

// ---------- Locking implementation ----------
type exclusiveLockerType struct {
	m sync.Map
}

var exclusiveLocker = newExclusiveLocker()

func (e *exclusiveLockerType) lock(lockKey lockKeyType) unlocker {
	logger.Debugf("Acquiring lock for %s", lockKey.lockID())
	// We can ignore clearing up the map because the number of unique lockID is very limited, and they will be probably reused during the lifetime of the app.
	mu, _ := e.m.LoadOrStore(lockKey.lockID(), &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex)
}

func newExclusiveLocker() *exclusiveLockerType {
	return &exclusiveLockerType{}
}

// ---------- No locking implementation ----------
type noopLockerType struct{}

var noopLocker = &noopLockerType{}

type noopUnlocker struct{}

func (n noopUnlocker) Unlock() {

}

func (n *noopLockerType) lock(_ lockKeyType) unlocker {
	logger.Debug("No lock is acquired")
	return noopUnlocker{}
}
