package storagetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type markerKey struct{}

func Test_Memory_StoresCopies(t *testing.T) {
	t.Parallel()
	var m Memory
	val := []byte("value")
	require.NoError(t, m.Set("k", val, time.Minute))
	val[0] = 'X'

	got, err := m.Get("k")
	require.NoError(t, err)
	require.Equal(t, []byte("value"), got)
	got[0] = 'Y'
	require.Equal(t, []byte("value"), m.Data["k"])

	missing, err := m.Get("missing")
	require.NoError(t, err)
	require.Nil(t, missing)
	require.True(t, m.Holds("k"))
	require.False(t, m.Holds("missing"))
}

func Test_Memory_DeleteResetAndKeys(t *testing.T) {
	t.Parallel()
	m := New()
	for _, key := range []string{"b", "c", "a"} {
		require.NoError(t, m.Set(key, []byte(key), 0))
	}
	require.Equal(t, []string{"a", "b", "c"}, m.Keys())

	require.NoError(t, m.Delete("b"))
	require.Equal(t, []string{"a", "c"}, m.Keys())

	m.SetErr(OpGet, "a", errors.New("boom"))
	require.NoError(t, m.Reset())
	require.Empty(t, m.Keys())
	require.Empty(t, m.Errs)
	require.NoError(t, m.Close())
}

func Test_Memory_ErrorTable(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	errAny := errors.New("any")

	get := func(m *Memory) error { _, err := m.Get("k"); return err }
	set := func(m *Memory) error { return m.Set("k", []byte("new"), 0) }
	del := func(m *Memory) error { return m.Delete("k") }
	reset := func(m *Memory) error { return m.Reset() }
	for _, tc := range []struct {
		call func(*Memory) error
		name string
		op   string
		key  string
	}{
		{name: "get by key", op: OpGet, key: "k", call: get},
		{name: "set by key", op: OpSet, key: "k", call: set},
		{name: "delete by key", op: OpDelete, key: "k", call: del},
		{name: "reset", op: OpReset, call: reset},
		{name: "get on any key", op: OpGet, key: "*", call: get},
		{name: "set on any key", op: OpSet, key: "*", call: set},
		{name: "delete on any key", op: OpDelete, key: "*", call: del},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := New()
			require.NoError(t, m.Set("k", []byte("old"), 0))
			m.SetErr(tc.op, tc.key, errBoom)
			require.ErrorIs(t, tc.call(m), errBoom)
			require.Equal(t, []byte("old"), m.Data["k"], "a failed operation must not change the data")
		})
	}

	t.Run("the exact key wins over the wildcard", func(t *testing.T) {
		t.Parallel()
		m := New().SetErr(OpGet, "*", errAny).SetErr(OpGet, "k", errBoom)
		_, err := m.Get("k")
		require.ErrorIs(t, err, errBoom)
		_, err = m.Get("other")
		require.ErrorIs(t, err, errAny)
	})

	t.Run("a nil error clears the entry", func(t *testing.T) {
		t.Parallel()
		m := New().SetErr(OpSet, "k", errBoom)
		require.ErrorIs(t, m.Set("k", nil, 0), errBoom)
		m.SetErr(OpSet, "k", nil)
		require.NoError(t, m.Set("k", nil, 0))
	})

	t.Run("the zero value takes an error", func(t *testing.T) {
		t.Parallel()
		var m Memory
		m.SetErr(OpDelete, "*", errBoom)
		require.ErrorIs(t, m.Delete("anything"), errBoom)
	})
}

func Test_Memory_RecordsCalls(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	m := New().SetErr(OpSet, "bad", errBoom)

	require.NoError(t, m.Set("a", []byte("1"), time.Hour))
	require.ErrorIs(t, m.Set("bad", []byte("2"), time.Minute), errBoom)
	_, err := m.Get("a")
	require.NoError(t, err)
	require.NoError(t, m.Delete("a"))

	require.Equal(t, []Call{
		{Op: OpSet, Key: "a", Value: []byte("1"), TTL: time.Hour},
		{Op: OpSet, Key: "bad", Value: []byte("2"), TTL: time.Minute},
	}, m.Calls(OpSet), "failed calls are recorded too")
	require.Equal(t, 1, m.Count(OpGet))
	require.Equal(t, 1, m.Count(OpDelete))
	require.Equal(t, 0, m.Count(OpReset))

	m.ClearCalls()
	require.Zero(t, m.Count(OpSet))
	require.False(t, m.Holds("a"), "ClearCalls keeps the data, and a was deleted")
}

func Test_Memory_HonorContext(t *testing.T) {
	t.Parallel()
	canceledContext := func(t *testing.T) context.Context {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}

	for _, tc := range []struct {
		context func(t *testing.T) context.Context
		want    error
		name    string
	}{
		{name: "canceled", context: canceledContext, want: context.Canceled},
		{
			name: "expired",
			context: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Hour))
				t.Cleanup(cancel)
				return ctx
			},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := tc.context(t)
			m := &Memory{HonorContext: true}
			_, err := m.GetWithContext(ctx, "k")
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, m.SetWithContext(ctx, "k", []byte("v"), 0), tc.want)
			require.ErrorIs(t, m.DeleteWithContext(ctx, "k"), tc.want)
			require.ErrorIs(t, m.ResetWithContext(ctx), tc.want)
			require.False(t, m.Holds("k"))
			require.Zero(t, m.Count(OpSet), "a call refused for its context is not recorded")
		})
	}

	t.Run("off by default", func(t *testing.T) {
		t.Parallel()
		m := New()
		require.NoError(t, m.SetWithContext(canceledContext(t), "k", []byte("v"), 0))
		require.True(t, m.Holds("k"))
	})
}

func Test_Memory_RecordsContext(t *testing.T) {
	t.Parallel()
	m := NewMarked(markerKey{})
	marked, cancel := context.WithCancel(context.WithValue(context.Background(), markerKey{}, "label"))
	cancel()
	plain := context.Background()

	require.NoError(t, m.SetWithContext(marked, "a", []byte("1"), 0))
	_, err := m.GetWithContext(marked, "a")
	require.NoError(t, err)
	_, err = m.GetWithContext(plain, "b")
	require.NoError(t, err)
	require.NoError(t, m.DeleteWithContext(plain, "a"))

	require.Equal(t, []Call{{Op: OpSet, Key: "a", Value: []byte("1"), Marker: "label", Canceled: true}}, m.Calls(OpSet))
	require.Equal(t, []Call{{Op: OpGet, Key: "a", Marker: "label", Canceled: true}, {Op: OpGet, Key: "b"}}, m.Calls(OpGet))
	require.Equal(t, []Call{{Op: OpDelete, Key: "a"}}, m.Calls(OpDelete))

	require.NoError(t, m.Set("c", []byte("3"), 0))
	require.Equal(t, Call{Op: OpSet, Key: "c", Value: []byte("3")}, m.Calls(OpSet)[1], "a call without a context is a background one")

	unmarked := New()
	require.NoError(t, unmarked.SetWithContext(marked, "a", nil, 0))
	require.Equal(t, Call{Op: OpSet, Key: "a", Canceled: true}, unmarked.Calls(OpSet)[0], "no marker key, no marker")
}

func Test_Failing(t *testing.T) {
	t.Parallel()
	errBoom, errClose := errors.New("boom"), errors.New("close")

	for _, tc := range []struct {
		err  error
		name string
	}{
		{name: "error", err: errBoom},
		{name: "nil error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &Failing{Err: tc.err, CloseErr: errClose}
			ctx := context.Background()

			got, err := f.GetWithContext(ctx, "k")
			require.Nil(t, got)
			require.ErrorIs(t, err, tc.err)
			got, err = f.Get("k")
			require.Nil(t, got)
			require.ErrorIs(t, err, tc.err)
			for i, err := range []error{
				f.SetWithContext(ctx, "k", nil, 0), f.Set("k", nil, 0), f.DeleteWithContext(ctx, "k"), f.Delete("k"),
				f.ResetWithContext(ctx), f.Reset(),
			} {
				if tc.err == nil {
					require.NoError(t, err, "operation %d", i)
				} else {
					require.ErrorIs(t, err, tc.err, "operation %d", i)
				}
			}

			require.False(t, f.Closed())
			require.ErrorIs(t, f.Close(), errClose)
			require.True(t, f.Closed())
		})
	}
}
