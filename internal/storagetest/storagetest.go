// Package storagetest provides the fiber.Storage fakes that the tests of the
// root package and of the storage-backed middleware keep needing: a map-backed
// storage with a per-operation error table and call recording, which also
// records the context each call received, and a storage that always fails.
// The fakes ignore expirations; a test that needs entries to expire has to use
// internal/storage/memory.
//
// Like internal/apptest, this package does not import fiber, so the in-package
// tests of the fiber package itself can use it without an import cycle. Every
// type has the full fiber.Storage method set, including the *WithContext
// variants, Reset and Close.
package storagetest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// The operations whose errors Memory.SetErr configures and whose calls
// Memory.Calls reports.
const (
	OpGet    = "get"
	OpSet    = "set"
	OpDelete = "del"
	OpReset  = "reset"
)

// Call is one operation a Memory served.
type Call struct {
	// Op is OpGet, OpSet, OpDelete or OpReset.
	Op string
	// Key is the key the call named, empty for OpReset.
	Key string
	// Marker is the string the call's context holds under Memory.MarkerKey, or empty.
	Marker string
	// Value is a copy of the value an OpSet stored, nil for the other operations.
	Value []byte
	// TTL is the expiration an OpSet was given, which Memory otherwise ignores.
	TTL time.Duration
	// Canceled reports whether the call's context was already canceled. The
	// methods without a context use a background context, which never is.
	Canceled bool
}

// Memory is a map-backed storage for tests. Its zero value is ready to use.
//
// Values are copied on Set and on Get, and expirations are ignored. An
// operation fails with the error stored in Errs under "<op>|<key>", or else
// under "<op>|*" for any key, where op is one of the Op constants; SetErr
// fills the table. Every call is recorded, failed or not.
//
// The methods lock Mu. Tests may read and write Data and Errs directly while
// no request is in flight, or while holding Mu.
type Memory struct {
	// MarkerKey is the context key whose string value Call.Marker reports, nil for none.
	MarkerKey any
	// Data holds the stored values by key.
	Data map[string][]byte
	// Errs maps "<op>|<key>" and "<op>|*" to the error that operation returns.
	Errs map[string]error
	// calls is the call log, guarded by Mu.
	calls []Call
	// Mu guards Data, Errs and the call log.
	Mu sync.RWMutex
	// HonorContext makes every *WithContext method fail with the error of an
	// already canceled or expired context before it does anything else.
	HonorContext bool
}

// New returns an empty Memory with an initialized error table, for tests that
// write Errs directly.
func New() *Memory {
	return &Memory{Data: make(map[string][]byte), Errs: make(map[string]error)}
}

// NewMarked returns an empty Memory like New that reports, for every call, the
// string its context holds under markerKey.
func NewMarked(markerKey any) *Memory {
	m := New()
	m.MarkerKey = markerKey
	return m
}

// SetErr makes op on key fail with err, key "*" making it fail on every key,
// or succeed again when err is nil. It returns m to chain.
func (m *Memory) SetErr(op, key string, err error) *Memory {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if err == nil {
		delete(m.Errs, op+"|"+key)
		return m
	}
	if m.Errs == nil {
		m.Errs = make(map[string]error)
	}
	m.Errs[op+"|"+key] = err
	return m
}

// Holds reports whether m stores a value under key.
func (m *Memory) Holds(key string) bool {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	_, ok := m.Data[key]
	return ok
}

// Keys returns the stored keys in sorted order.
func (m *Memory) Keys() []string {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	return slices.Sorted(maps.Keys(m.Data))
}

// Calls returns the recorded calls of op in order.
func (m *Memory) Calls(op string) []Call {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	var out []Call
	for _, c := range m.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// Count returns how many calls of op were recorded.
func (m *Memory) Count(op string) int { return len(m.Calls(op)) }

// ClearCalls forgets the recorded calls and keeps the data.
func (m *Memory) ClearCalls() {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.calls = nil
}

// begin records c and returns the error it has to fail with. The caller holds Mu.
func (m *Memory) begin(ctx context.Context, c *Call) error {
	if m.HonorContext {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // the fake returns the context error verbatim
		}
	}
	c.Canceled = errors.Is(ctx.Err(), context.Canceled)
	if value, ok := ctx.Value(m.MarkerKey).(string); ok && m.MarkerKey != nil {
		c.Marker = value
	}
	m.calls = append(m.calls, *c)
	if err := m.Errs[c.Op+"|"+c.Key]; err != nil {
		return err
	}
	return m.Errs[c.Op+"|*"]
}

// GetWithContext returns a copy of the value under key, or nil when there is none.
func (m *Memory) GetWithContext(ctx context.Context, key string) ([]byte, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if err := m.begin(ctx, &Call{Op: OpGet, Key: key}); err != nil {
		return nil, err
	}
	return append([]byte(nil), m.Data[key]...), nil
}

// Get is GetWithContext with a background context.
func (m *Memory) Get(key string) ([]byte, error) { return m.GetWithContext(context.Background(), key) }

// SetWithContext stores a copy of val under key and ignores exp.
func (m *Memory) SetWithContext(ctx context.Context, key string, val []byte, exp time.Duration) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if err := m.begin(ctx, &Call{Op: OpSet, Key: key, Value: append([]byte(nil), val...), TTL: exp}); err != nil {
		return err
	}
	if m.Data == nil {
		m.Data = make(map[string][]byte)
	}
	m.Data[key] = append([]byte(nil), val...)
	return nil
}

// Set is SetWithContext with a background context.
func (m *Memory) Set(key string, val []byte, exp time.Duration) error {
	return m.SetWithContext(context.Background(), key, val, exp)
}

// DeleteWithContext removes key.
func (m *Memory) DeleteWithContext(ctx context.Context, key string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if err := m.begin(ctx, &Call{Op: OpDelete, Key: key}); err != nil {
		return err
	}
	delete(m.Data, key)
	return nil
}

// Delete is DeleteWithContext with a background context.
func (m *Memory) Delete(key string) error { return m.DeleteWithContext(context.Background(), key) }

// ResetWithContext drops all data and clears the error table.
func (m *Memory) ResetWithContext(ctx context.Context) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if err := m.begin(ctx, &Call{Op: OpReset}); err != nil {
		return err
	}
	m.Data = make(map[string][]byte)
	m.Errs = make(map[string]error)
	return nil
}

// Reset is ResetWithContext with a background context.
func (m *Memory) Reset() error { return m.ResetWithContext(context.Background()) }

// Close does nothing and keeps the data.
func (*Memory) Close() error { return nil }

// Failing is a storage whose every operation fails with Err, which may be nil.
type Failing struct {
	// Err is what Get, Set, Delete and Reset return, with or without a context.
	Err error
	// CloseErr is what Close returns.
	CloseErr error
	closed   atomic.Bool
}

// Closed reports whether Close was called.
func (f *Failing) Closed() bool { return f.closed.Load() }

// GetWithContext fails with Err.
func (f *Failing) GetWithContext(context.Context, string) ([]byte, error) { return nil, f.Err }

// Get fails with Err.
func (f *Failing) Get(string) ([]byte, error) { return nil, f.Err }

// SetWithContext fails with Err.
func (f *Failing) SetWithContext(context.Context, string, []byte, time.Duration) error { return f.Err }

// Set fails with Err.
func (f *Failing) Set(string, []byte, time.Duration) error { return f.Err }

// DeleteWithContext fails with Err.
func (f *Failing) DeleteWithContext(context.Context, string) error { return f.Err }

// Delete fails with Err.
func (f *Failing) Delete(string) error { return f.Err }

// ResetWithContext fails with Err.
func (f *Failing) ResetWithContext(context.Context) error { return f.Err }

// Reset fails with Err.
func (f *Failing) Reset() error { return f.Err }

// Close records that it ran and returns CloseErr.
func (f *Failing) Close() error {
	f.closed.Store(true)
	return f.CloseErr
}
