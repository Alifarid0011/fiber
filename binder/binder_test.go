package binder

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func Test_GetAndPutToThePool(t *testing.T) {
	t.Parallel()

	// Panics in case we get from another pool
	require.Panics(t, func() { _ = GetFromThePool[*HeaderBinding](&CookieBinderPool) })

	// We get from the pool
	binder := GetFromThePool[*HeaderBinding](&HeaderBinderPool)
	PutToThePool(&HeaderBinderPool, binder)

	_ = GetFromThePool[*RespHeaderBinding](&RespHeaderBinderPool)
	_ = GetFromThePool[*QueryBinding](&QueryBinderPool)
	_ = GetFromThePool[*FormBinding](&FormBinderPool)
	_ = GetFromThePool[*URIBinding](&URIBinderPool)
	_ = GetFromThePool[*XMLBinding](&XMLBinderPool)
	_ = GetFromThePool[*JSONBinding](&JSONBinderPool)
	_ = GetFromThePool[*CBORBinding](&CBORBinderPool)
	_ = GetFromThePool[*MsgPackBinding](&MsgPackBinderPool)
}

func Test_Binders_ErrorPaths(t *testing.T) {
	t.Run("query binder invalid key", func(t *testing.T) {
		b := &QueryBinding{}
		req := newQueryRequest(t, "invalid[%3Dval&name=john")
		err := b.Bind(req, &struct{}{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unmatched brackets")
	})

	t.Run("form binder invalid key", func(t *testing.T) {
		b := &FormBinding{}
		req := newFormRequest(t, "invalid[=val")
		err := b.Bind(req, &struct{}{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unmatched brackets")
	})

	t.Run("form binder bad multipart", func(t *testing.T) {
		b := &FormBinding{}
		req := acquireRequest(t)
		req.Header.SetContentType(MIMEMultipartForm)
		err := b.Bind(req, &struct{}{})
		require.Error(t, err)
	})
}

// resettable is what the name and reset checks need of a binder.
type resettable interface {
	Name() string
	Reset()
}

// requireNameAndReset requires b to report name, and Reset to clear the
// splitting flag that points into b.
func requireNameAndReset(t *testing.T, b resettable, name string, splitting *bool) {
	t.Helper()

	require.Equal(t, name, b.Name())

	b.Reset()
	require.False(t, *splitting)
}

func Test_Binders_NameAndReset(t *testing.T) {
	t.Parallel()
	t.Run("query", func(t *testing.T) {
		t.Parallel()
		b := &QueryBinding{EnableSplitting: true}
		requireNameAndReset(t, b, "query", &b.EnableSplitting)
	})

	t.Run("form", func(t *testing.T) {
		t.Parallel()
		b := &FormBinding{EnableSplitting: true}
		requireNameAndReset(t, b, "form", &b.EnableSplitting)
	})

	t.Run("header", func(t *testing.T) {
		t.Parallel()
		b := &HeaderBinding{EnableSplitting: true}
		requireNameAndReset(t, b, "header", &b.EnableSplitting)
	})

	t.Run("resp header", func(t *testing.T) {
		t.Parallel()
		b := &RespHeaderBinding{EnableSplitting: true}
		requireNameAndReset(t, b, "respHeader", &b.EnableSplitting)
	})

	t.Run("cookie", func(t *testing.T) {
		t.Parallel()
		b := &CookieBinding{EnableSplitting: true}
		requireNameAndReset(t, b, "cookie", &b.EnableSplitting)
	})
}

func Test_GetFieldCache_Panic(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { getFieldCache("unknown") })
}

func Test_parseToMap_defaultCase(t *testing.T) {
	t.Parallel()
	m := map[string]int{}
	err := parseToMap(reflect.ValueOf(m), map[string][]string{"a": {"1"}})
	require.NoError(t, err)
	require.Empty(t, m)

	m2 := map[string]string{}
	err = parseToMap(reflect.ValueOf(m2), map[string][]string{"empty": {}})
	require.NoError(t, err)
	require.Empty(t, m2["empty"])

	var zeroStringMap map[string]string
	err = parseToMap(reflect.ValueOf(&zeroStringMap).Elem(), map[string][]string{"name": {"john"}})
	require.NoError(t, err)
	require.Equal(t, "john", zeroStringMap["name"])
}

func Test_parse_function_maps(t *testing.T) {
	t.Parallel()
	m := map[string][]string{}
	err := parse("query", &m, map[string][]string{"a": {"b"}})
	require.NoError(t, err)
	require.Equal(t, []string{"b"}, m["a"])

	m2 := map[string]string{}
	err = parse("query", &m2, map[string][]string{"a": {"b"}})
	require.NoError(t, err)
	require.Equal(t, "b", m2["a"])

	var zeroStringMap map[string]string
	err = parse("query", &zeroStringMap, map[string][]string{"foo": {"bar", "baz"}})
	require.NoError(t, err)
	require.Equal(t, "baz", zeroStringMap["foo"])

	var zeroSliceMap map[string][]string
	err = parse("query", &zeroSliceMap, map[string][]string{"foo": {"bar", "baz"}})
	require.NoError(t, err)
	require.Equal(t, []string{"bar", "baz"}, zeroSliceMap["foo"])
}

func Test_SetParserDecoder_UnknownKeys(t *testing.T) {
	SetParserDecoder(ParserConfig{IgnoreUnknownKeys: false})
	defer SetParserDecoder(ParserConfig{IgnoreUnknownKeys: true, ZeroEmpty: true})
	type user struct {
		Name string `query:"name"`
	}
	data := map[string][]string{"name": {"john"}, "foo": {"bar"}}
	err := parseToStruct("query", &user{}, data)
	require.Error(t, err)

	// The binders decode a struct from its pairs with DecodeValues, which must
	// honor the setting as well.
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.URI().SetQueryString("name=john&foo=bar")
	require.ErrorContains(t, (&QueryBinding{}).Bind(req, &user{}), "foo")
	require.ErrorContains(t, parseValuesToStruct("query", &user{}, []string{"name", "foo"}, []string{"john", "bar"}), "foo")

	SetParserDecoder(ParserConfig{IgnoreUnknownKeys: true, ZeroEmpty: true})
	var u user
	require.NoError(t, (&QueryBinding{}).Bind(req, &u))
	require.Equal(t, "john", u.Name)
}

func Test_SetParserDecoder_CustomConverter(t *testing.T) {
	type myInt int
	conv := func(s string) reflect.Value {
		v, _ := strconv.Atoi(s) //nolint:errcheck // not needed
		mi := myInt(v)
		return reflect.ValueOf(mi)
	}

	SetParserDecoder(ParserConfig{ParserType: []ParserType{{CustomType: myInt(0), Converter: conv}}})
	defer SetParserDecoder(ParserConfig{IgnoreUnknownKeys: true, ZeroEmpty: true})

	type data struct {
		V myInt `query:"v"`
	}
	d := new(data)
	err := parse("query", d, map[string][]string{"v": {"5"}})
	require.NoError(t, err)
	require.Equal(t, myInt(5), d.V)
}

// Test_SetParserDecoder_AliasTagIgnored: ParserConfig.SetAliasTag must not
// override the per-source binding tag (one global tag breaks multi-source binding).
func Test_SetParserDecoder_AliasTagIgnored(t *testing.T) {
	SetParserDecoder(ParserConfig{SetAliasTag: "ignored", IgnoreUnknownKeys: true, ZeroEmpty: true})
	defer SetParserDecoder(ParserConfig{IgnoreUnknownKeys: true, ZeroEmpty: true})

	type data struct {
		Greeting string `query:"v"`
	}
	d := new(data)
	err := parse("query", d, map[string][]string{"v": {"hello"}})
	require.NoError(t, err)
	require.Equal(t, "hello", d.Greeting)
}
