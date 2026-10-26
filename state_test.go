package fiber

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestState_SetAndGet_WithApp(t *testing.T) {
	t.Parallel()
	// Create app
	app := New()

	// test setting and getting a value
	app.State().Set("foo", "bar")
	val, ok := app.State().Get("foo")
	require.True(t, ok)
	require.Equal(t, "bar", val)

	// test key not found
	_, ok = app.State().Get("unknown")
	require.False(t, ok)
}

func TestState_SetAndGet(t *testing.T) {
	t.Parallel()
	st := newState()

	// test setting and getting a value
	st.Set("foo", "bar")
	val, ok := st.Get("foo")
	require.True(t, ok)
	require.Equal(t, "bar", val)

	// test key not found
	_, ok = st.Get("unknown")
	require.False(t, ok)
}

// stateTestGetter runs the three scenarios every typed State getter must satisfy, each as its own
// subtest: a stored value of the getter's type is returned together with true, while a value of
// another type (wrongType) and a missing key both return the zero value together with false.
//
// State hands values back untouched, so they are compared with Equal even for floats.
func stateTestGetter[T any](t *testing.T, get func(*State, string) (T, bool), value T, wrongType any) {
	t.Helper()

	var zero T
	for _, tc := range []struct {
		want T
		name string
		key  string
		ok   bool
	}{
		{name: "stored value", key: "value", want: value, ok: true},
		{name: "wrong type", key: "wrong", want: zero},
		{name: "missing key", key: "missing", want: zero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := newState()
			st.Set("value", value)
			st.Set("wrong", wrongType)

			got, ok := get(st, tc.key)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestState_GetString(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetString, "hello", 123)
}

func TestState_GetInt(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetInt, 456, "abc")
}

func TestState_GetBool(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetBool, true, 1)
}

func TestState_GetFloat64(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetFloat64, 3.14, 10)
}

func TestState_GetUint(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetUint, 100, "not uint")
}

func TestState_GetInt8(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetInt8, 10, "not int8")
}

func TestState_GetInt16(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetInt16, 200, "not int16")
}

func TestState_GetInt32(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetInt32, 3000, "not int32")
}

func TestState_GetInt64(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetInt64, 4000, "not int64")
}

func TestState_GetUint8(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetUint8, 20, "not uint8")
}

func TestState_GetUint16(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetUint16, 300, "not uint16")
}

func TestState_GetUint32(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetUint32, 400000, "not uint32")
}

func TestState_GetUint64(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetUint64, 5000000, "not uint64")
}

func TestState_GetUintptr(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetUintptr, 12345, "not uintptr")
}

func TestState_GetFloat32(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetFloat32, 3.14, "not float32")
}

func TestState_GetComplex64(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetComplex64, complex(2, 3), "not complex64")
}

func TestState_GetComplex128(t *testing.T) {
	t.Parallel()
	stateTestGetter(t, (*State).GetComplex128, complex(4, 5), "not complex128")
}

func TestState_MustGet(t *testing.T) {
	t.Parallel()
	st := newState()

	st.Set("exists", "value")
	val := st.MustGet("exists")
	require.Equal(t, "value", val)

	// must-get on missing key should panic
	require.Panics(t, func() {
		_ = st.MustGet("missing")
	})
}

func TestState_Has(t *testing.T) {
	t.Parallel()

	st := newState()

	st.Set("key", "value")
	require.True(t, st.Has("key"))
}

func TestState_Delete(t *testing.T) {
	t.Parallel()
	st := newState()

	st.Set("key", "value")
	st.Delete("key")
	_, ok := st.Get("key")
	require.False(t, ok)
}

func TestState_Reset(t *testing.T) {
	t.Parallel()
	st := newState()

	st.Set("a", 1)
	st.Set("b", 2)
	st.Reset()
	require.Equal(t, 0, st.Len())
	require.Empty(t, st.Keys())
}

func TestState_Keys(t *testing.T) {
	t.Parallel()
	st := newState()

	keys := []string{"one", "two", "three"}
	for _, k := range keys {
		st.Set(k, k)
	}

	returnedKeys := st.Keys()
	require.ElementsMatch(t, keys, returnedKeys)
}

func TestState_Keys_SkipsNonStringKeys(t *testing.T) {
	t.Parallel()

	st := newState()
	st.Set("one", "one")
	st.Set("two", "two")
	st.dependencies.Store(42, "value")
	st.dependencies.Store(struct{}{}, "value")

	returnedKeys := st.Keys()
	require.ElementsMatch(t, []string{"one", "two"}, returnedKeys)
}

func TestState_Keys_SkipsNonStringKeys_WithMixedOrder(t *testing.T) {
	t.Parallel()

	st := newState()
	st.Set("before", "value")
	st.dependencies.Store(42, "value")
	st.Set("after", "value")

	returnedKeys := st.Keys()
	require.ElementsMatch(t, []string{"before", "after"}, returnedKeys)
}

func TestState_Len(t *testing.T) {
	t.Parallel()
	st := newState()

	require.Equal(t, 0, st.Len())

	st.Set("a", "a")
	require.Equal(t, 1, st.Len())

	st.Set("b", "b")
	require.Equal(t, 2, st.Len())

	st.Delete("a")
	require.Equal(t, 1, st.Len())
}

type testCase[T any] struct {
	value    any
	expected T
	name     string
	key      string
	ok       bool
}

func runGenericTest[T any](t *testing.T, getter func(*State, string) (T, bool), tests []testCase[T]) {
	t.Helper()

	st := newState()
	for _, tc := range tests {
		st.Set(tc.key, tc.value)
		got, ok := getter(st, tc.key)
		require.Equal(t, tc.ok, ok, tc.name)
		require.Equal(t, tc.expected, got, tc.name)
	}
}

func TestState_GetGeneric(t *testing.T) {
	t.Parallel()

	runGenericTest(t, GetState[int], []testCase[int]{
		{name: "int correct conversion", key: "num", value: 42, expected: 42, ok: true},
		{name: "int wrong conversion from string", key: "str", value: "abc", expected: 0, ok: false},
	})

	runGenericTest(t, GetState[string], []testCase[string]{
		{name: "string correct conversion", key: "strVal", value: "hello", expected: "hello", ok: true},
		{name: "string wrong conversion from int", key: "intVal", value: 100, expected: "", ok: false},
	})

	runGenericTest(t, GetState[bool], []testCase[bool]{
		{name: "bool correct conversion", key: "flag", value: true, expected: true, ok: true},
		{name: "bool wrong conversion from int", key: "intFlag", value: 1, expected: false, ok: false},
	})

	runGenericTest(t, GetState[float64], []testCase[float64]{
		{name: "float64 correct conversion", key: "pi", value: 3.14, expected: 3.14, ok: true},
		{name: "float64 wrong conversion from int", key: "intVal", value: 10, expected: 0.0, ok: false},
	})
}

func Test_MustGetStateGeneric(t *testing.T) {
	t.Parallel()
	st := newState()

	st.Set("flag", true)
	flag := MustGetState[bool](st, "flag")
	require.True(t, flag)

	// mismatched type should panic
	require.Panics(t, func() {
		_ = MustGetState[string](st, "flag")
	})

	// missing key should also panic
	require.Panics(t, func() {
		_ = MustGetState[string](st, "missing")
	})
}

func Test_GetStateWithDefault(t *testing.T) {
	t.Parallel()
	st := newState()

	st.Set("flag", true)
	flag := GetStateWithDefault(st, "flag", false)
	require.True(t, flag)

	// mismatched type should return the default value
	str := GetStateWithDefault(st, "flag", "default")
	require.Equal(t, "default", str)

	// missing key should return the default value
	flag = GetStateWithDefault(st, "missing", false)
	require.False(t, flag)
}

func TestState_Service(t *testing.T) {
	t.Parallel()

	srv1 := &mockService{name: "test1"}
	// service 2 is using a very subtle name to check it is not picked up
	srv2 := &mockService{name: "test1 "}

	t.Run("set/get/ok", func(t *testing.T) {
		t.Parallel()

		st := newState()
		st.setService(srv1)

		got, ok := st.Get(st.serviceKey(srv1.String()))
		require.True(t, ok)
		require.Equal(t, srv1, got)
	})

	t.Run("set/get/ko", func(t *testing.T) {
		t.Parallel()

		st := newState()
		st.setService(srv1)

		koSrv := &mockService{name: "ko"}

		got, ok := st.Get(st.serviceKey(koSrv.String()))
		require.False(t, ok)
		require.Nil(t, got)
	})

	t.Run("len", func(t *testing.T) {
		t.Parallel()

		t.Run("empty", func(t *testing.T) {
			t.Parallel()

			st := newState()
			require.Equal(t, 0, st.Len())
			require.Empty(t, st.serviceKeys())
		})

		t.Run("with-services", func(t *testing.T) {
			t.Parallel()

			st := newState()
			st.setService(srv1)
			st.setService(srv2)

			require.Equal(t, 2, st.Len())
			require.Equal(t, 2, st.ServicesLen())
		})

		t.Run("with-services/with-keys", func(t *testing.T) {
			t.Parallel()

			st := newState()
			st.setService(srv1)
			st.setService(srv2)
			st.Set("key1", "value1")
			st.Set("key2", "value2")

			servicesLen := st.ServicesLen()
			require.Equal(t, 4, st.Len())
			require.Equal(t, 2, servicesLen)
		})
	})

	t.Run("keys", func(t *testing.T) {
		t.Run("empty", func(t *testing.T) {
			t.Parallel()

			st := newState()
			// adding more keys to check they are not included
			st.Set("key1", "value1")
			st.Set("key2", "value2")

			require.Empty(t, st.serviceKeys())
		})

		t.Run("with-services", func(t *testing.T) {
			t.Parallel()

			st := newState()
			st.setService(srv1)
			st.setService(srv2)

			keys := st.serviceKeys()
			require.Len(t, keys, 2)
			require.Contains(t, keys, st.serviceKey(srv1.String()))
			require.Contains(t, keys, st.serviceKey(srv2.String()))
		})

		t.Run("with-services/with-keys", func(t *testing.T) {
			t.Parallel()

			st := newState()
			st.setService(srv1)
			st.setService(srv2)
			st.Set("key1", "value1")
			st.Set("key2", "value2")

			keys := st.serviceKeys()
			require.Len(t, keys, 2)
			require.Contains(t, keys, st.serviceKey(srv1.String()))
			require.Contains(t, keys, st.serviceKey(srv2.String()))
			require.NotContains(t, keys, "key1")
			require.NotContains(t, keys, "key2")
		})

		t.Run("with-non-string-key", func(t *testing.T) {
			t.Parallel()

			st := newState()
			st.setService(srv1)
			st.setService(srv2)
			st.dependencies.Store(42, "value")
			st.dependencies.Store(struct{}{}, "value")

			keys := st.serviceKeys()
			require.Len(t, keys, 2)
			require.Contains(t, keys, st.serviceKey(srv1.String()))
			require.Contains(t, keys, st.serviceKey(srv2.String()))
		})

		t.Run("with-non-service-keys", func(t *testing.T) {
			t.Parallel()

			st := newState()
			st.setService(srv1)
			st.setService(srv2)
			st.Set("other", "value")
			st.dependencies.Store(42, "value")

			keys := st.serviceKeys()
			require.Len(t, keys, 2)
			require.Contains(t, keys, st.serviceKey(srv1.String()))
			require.Contains(t, keys, st.serviceKey(srv2.String()))
		})
	})

	t.Run("delete", func(t *testing.T) {
		t.Parallel()

		t.Run("ok", func(t *testing.T) {
			t.Parallel()

			st := newState()

			st.setService(srv1)
			st.deleteService(srv1)

			_, ok := st.Get(st.serviceKey(srv1.String()))
			require.False(t, ok)
		})

		t.Run("missing", func(t *testing.T) {
			t.Parallel()

			st := newState()
			st.setService(srv1)

			st.deleteService(srv2)

			_, ok := st.Get(st.serviceKey(srv1.String()))
			require.True(t, ok)

			_, ok = st.Get(st.serviceKey(srv2.String()))
			require.False(t, ok)
		})
	})
}

func TestState_GetService(t *testing.T) {
	t.Parallel()

	t.Run("ok", func(t *testing.T) {
		t.Parallel()

		srv1 := &mockService{name: "test1"}

		st := newState()
		st.setService(srv1)

		got, ok := GetService[*mockService](st, srv1.String())
		require.True(t, ok)
		require.Equal(t, srv1, got)
	})

	t.Run("ko", func(t *testing.T) {
		t.Parallel()

		srv1 := &mockService{name: "test1"}

		st := newState()

		got, ok := GetService[*mockService](st, srv1.String())
		require.False(t, ok)
		require.Nil(t, got)
	})
}

func TestState_MustGetService(t *testing.T) {
	t.Parallel()

	t.Run("ok", func(t *testing.T) {
		t.Parallel()

		srv1 := &mockService{name: "test1"}

		st := newState()
		st.setService(srv1)

		got := MustGetService[*mockService](st, srv1.String())
		require.Equal(t, srv1, got)
	})

	t.Run("panics", func(t *testing.T) {
		t.Parallel()

		srv1 := &mockService{name: "test1"}

		st := newState()

		require.Panics(t, func() {
			_ = MustGetService[*mockService](st, srv1.String())
		})
	})
}

func BenchmarkState_Set(b *testing.B) {
	b.ReportAllocs()

	st := newState()

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i)
		st.Set(key, i)
	}
}

func BenchmarkState_Get(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, i)
	}

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.Get(key)
	}
}

func BenchmarkState_GetString(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, strconv.Itoa(i))
	}

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetString(key)
	}
}

func BenchmarkState_GetInt(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, i)
	}

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetInt(key)
	}
}

func BenchmarkState_GetBool(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, i%2 == 0)
	}

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetBool(key)
	}
}

func BenchmarkState_GetFloat64(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, float64(i))
	}

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetFloat64(key)
	}
}

func BenchmarkState_MustGet(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, i)
	}

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.MustGet(key)
	}
}

func BenchmarkState_GetStateGeneric(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, i)
	}

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		GetState[int](st, key)
	}
}

func BenchmarkState_MustGetStateGeneric(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, i)
	}

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		MustGetState[int](st, key)
	}
}

func BenchmarkState_GetStateWithDefault(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, i)
	}

	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		GetStateWithDefault(st, key, 0)
	}
}

func BenchmarkState_Has(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	// prepopulate the state
	for i := range n {
		st.Set("key"+strconv.Itoa(i), i)
	}

	i := 0
	for b.Loop() {
		i++
		st.Has("key" + strconv.Itoa(i%n))
	}
}

func BenchmarkState_Delete(b *testing.B) {
	b.ReportAllocs()

	for b.Loop() {
		st := newState()
		st.Set("a", 1)
		st.Delete("a")
	}
}

func BenchmarkState_Reset(b *testing.B) {
	b.ReportAllocs()

	for b.Loop() {
		st := newState()
		// add a fixed number of keys before clearing
		for j := range 100 {
			st.Set("key"+strconv.Itoa(j), j)
		}
		st.Reset()
	}
}

func BenchmarkState_Keys(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	for i := range n {
		st.Set("key"+strconv.Itoa(i), i)
	}

	for b.Loop() {
		_ = st.Keys()
	}
}

func BenchmarkState_Len(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	n := 1000
	for i := range n {
		st.Set("key"+strconv.Itoa(i), i)
	}

	for b.Loop() {
		_ = st.Len()
	}
}

func BenchmarkState_GetUint(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with uint values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, uint(i))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetUint(key)
	}
}

func BenchmarkState_GetInt8(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with int8 values (using modulo to stay in range).
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, int8(i%128))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetInt8(key)
	}
}

func BenchmarkState_GetInt16(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with int16 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, int16(i))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetInt16(key)
	}
}

func BenchmarkState_GetInt32(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with int32 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, int32(i))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetInt32(key)
	}
}

func BenchmarkState_GetInt64(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with int64 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, int64(i))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetInt64(key)
	}
}

func BenchmarkState_GetUint8(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with uint8 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, uint8(i%256))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetUint8(key)
	}
}

func BenchmarkState_GetUint16(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with uint16 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, uint16(i))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetUint16(key)
	}
}

func BenchmarkState_GetUint32(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with uint32 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, uint32(i))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetUint32(key)
	}
}

func BenchmarkState_GetUint64(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with uint64 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, uint64(i))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetUint64(key)
	}
}

func BenchmarkState_GetUintptr(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with uintptr values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, uintptr(i))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetUintptr(key)
	}
}

func BenchmarkState_GetFloat32(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with float32 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		st.Set(key, float32(i))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetFloat32(key)
	}
}

func BenchmarkState_GetComplex64(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with complex64 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		// Create a complex64 value with both real and imaginary parts.
		st.Set(key, complex(float32(i), float32(i)))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetComplex64(key)
	}
}

func BenchmarkState_GetComplex128(b *testing.B) {
	b.ReportAllocs()
	st := newState()
	n := 1000
	// Prepopulate the state with complex128 values.
	for i := range n {
		key := "key" + strconv.Itoa(i)
		// Create a complex128 value with both real and imaginary parts.
		st.Set(key, complex(float64(i), float64(i)))
	}
	i := 0
	for b.Loop() {
		i++
		key := "key" + strconv.Itoa(i%n)
		st.GetComplex128(key)
	}
}

func BenchmarkState_GetService(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	srv := &mockService{name: "benchService"}
	st.setService(srv)

	for b.Loop() {
		_, _ = GetService[*mockService](st, srv.String())
	}
}

func BenchmarkState_MustGetService(b *testing.B) {
	b.ReportAllocs()

	st := newState()
	srv := &mockService{name: "benchService"}
	st.setService(srv)

	for b.Loop() {
		_ = MustGetService[*mockService](st, srv.String())
	}
}
