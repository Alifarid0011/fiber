package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"maps"
	"mime/multipart"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// clientTestBRequest returns a pooled request that is released when the test ends.
func clientTestBRequest(t *testing.T) *Request {
	t.Helper()

	req := AcquireRequest()
	t.Cleanup(func() { ReleaseRequest(req) })

	return req
}

// clientTestBMultiValue names the request methods that read and write one kind of
// multi-valued entry, so headers, query parameters and form fields run the same
// scenarios. The scenarios chain the calls through the returned request, as the
// fluent API is used. Headers have no struct or delete form: they leave setStruct
// and del nil and do not register the scenarios that need them.
type clientTestBMultiValue struct {
	add       func(r *Request, key, val string) *Request
	set       func(r *Request, key, val string) *Request
	addMany   func(r *Request, m map[string][]string) *Request
	setMany   func(r *Request, m map[string]string) *Request
	setStruct func(r *Request, v any) *Request
	del       func(r *Request, keys ...string) *Request
	get       func(r *Request, key string) []string
	noun      string // as in the scenario names: "header", "param" or "form field"
}

// run registers the scenarios that hold for every kind of entry.
func (e clientTestBMultiValue) run(t *testing.T) {
	t.Helper()

	t.Run("add "+e.noun, func(t *testing.T) {
		t.Parallel()

		req := clientTestBRequest(t)
		e.add(e.add(req, "foo", "bar"), "foo", "fiber")

		require.Equal(t, []string{"bar", "fiber"}, e.get(req, "foo"))
	})

	t.Run("set "+e.noun, func(t *testing.T) {
		t.Parallel()

		req := clientTestBRequest(t)
		e.set(e.add(req, "foo", "bar"), "foo", "fiber")

		require.Equal(t, []string{"fiber"}, e.get(req, "foo"))
	})

	t.Run("add "+e.noun+"s", func(t *testing.T) {
		t.Parallel()

		req := clientTestBRequest(t)
		e.addMany(e.set(req, "foo", "bar"), map[string][]string{"foo": {"fiber", "buaa"}, "bar": {"foo"}})

		require.Equal(t, []string{"bar", "fiber", "buaa"}, e.get(req, "foo"))
		require.Equal(t, []string{"foo"}, e.get(req, "bar"))
	})

	t.Run("set "+e.noun+"s", func(t *testing.T) {
		t.Parallel()

		req := clientTestBRequest(t)
		e.setMany(e.set(req, "foo", "bar"), map[string]string{"foo": "fiber", "bar": "foo"})

		require.Equal(t, []string{"fiber"}, e.get(req, "foo"))
		require.Equal(t, []string{"foo"}, e.get(req, "bar"))
	})
}

// runStruct registers the scenario that sets entries from a struct.
func (e clientTestBMultiValue) runStruct(t *testing.T) {
	t.Helper()

	// The struct carries the tag of every kind: each one reads only its own.
	type args struct {
		TString   string
		TSlice    []string
		TIntSlice []int `param:"int_slice" form:"int_slice"`
		TInt      int
		TFloat    float64
		TBool     bool
	}

	t.Run("set "+e.noun+"s with struct", func(t *testing.T) {
		t.Parallel()

		req := clientTestBRequest(t)
		e.setStruct(req, &args{
			TInt:      5,
			TString:   "string",
			TFloat:    3.1,
			TBool:     true,
			TSlice:    []string{"foo", "bar"},
			TIntSlice: []int{1, 2},
		})

		require.Empty(t, e.get(req, "unexport"))
		require.Equal(t, []string{"5"}, e.get(req, "TInt"))
		require.Equal(t, []string{"string"}, e.get(req, "TString"))
		require.Equal(t, []string{"3.1"}, e.get(req, "TFloat"))
		require.Equal(t, []string{"true"}, e.get(req, "TBool"))
		require.Equal(t, []string{"foo", "bar"}, e.get(req, "TSlice"))
		require.Equal(t, []string{"1", "2"}, e.get(req, "int_slice"))
	})
}

// runDel registers the scenario that deletes entries.
func (e clientTestBMultiValue) runDel(t *testing.T) {
	t.Helper()

	t.Run("del "+e.noun+"s", func(t *testing.T) {
		t.Parallel()

		req := clientTestBRequest(t)
		e.del(e.setMany(e.set(req, "foo", "bar"), map[string]string{"foo": "fiber", "bar": "foo"}), "foo", "bar")

		require.Empty(t, e.get(req, "foo"))
		require.Empty(t, e.get(req, "bar"))
	})
}

// runIter registers the scenarios of the iterator over all entries, which all
// returns for a request.
func (e clientTestBMultiValue) runIter(t *testing.T, all func(r *Request) iter.Seq2[string, []string]) {
	t.Helper()

	t.Run("empty iterator", func(t *testing.T) {
		t.Parallel()

		called := false
		all(clientTestBRequest(t))(func(_ string, _ []string) bool {
			called = true
			return true
		})

		require.False(t, called)
	})

	t.Run("populated iterator", func(t *testing.T) {
		t.Parallel()

		req := clientTestBRequest(t)
		e.addMany(req, map[string][]string{"foo": {"bar", "fiber"}, "bar": {"foo"}})

		got := maps.Collect(all(req))

		require.Contains(t, got["foo"], "bar")
		require.Contains(t, got["foo"], "fiber")
		require.Contains(t, got["bar"], "foo")

		require.Len(t, got, 2)
	})
}

// clientTestBSingleValue names the request methods that read and write one kind of
// single-valued entry, so cookies and path parameters run the same scenarios. The
// scenarios chain the calls through the returned request, as the fluent API is used.
type clientTestBSingleValue struct {
	get       func(r *Request, key string) string
	set       func(r *Request, key, val string) *Request
	setMany   func(r *Request, m map[string]string) *Request
	setStruct func(r *Request, v any) *Request
	del       func(r *Request, keys ...string) *Request
	noun      string // as in the scenario names: "cookie" or "path param"
}

// run registers the scenarios that hold for every kind of entry.
func (e clientTestBSingleValue) run(t *testing.T) {
	t.Helper()

	// The struct carries the tag of every kind: each one reads only its own.
	type args struct {
		String string `cookie:"string" path:"string"`
		Int    int    `cookie:"int" path:"int"`
	}

	t.Run("set "+e.noun, func(t *testing.T) {
		t.Parallel()

		req := e.set(clientTestBRequest(t), "foo", "bar")
		require.Equal(t, "bar", e.get(req, "foo"))

		e.set(req, "foo", "bar1")
		require.Equal(t, "bar1", e.get(req, "foo"))
	})

	t.Run("set "+e.noun+"s", func(t *testing.T) {
		t.Parallel()

		req := e.setMany(clientTestBRequest(t), map[string]string{"foo": "bar", "bar": "foo"})
		require.Equal(t, "bar", e.get(req, "foo"))
		require.Equal(t, "foo", e.get(req, "bar"))

		e.setMany(req, map[string]string{"foo": "bar1"})
		require.Equal(t, "bar1", e.get(req, "foo"))
		require.Equal(t, "foo", e.get(req, "bar"))
	})

	t.Run("set "+e.noun+"s with struct", func(t *testing.T) {
		t.Parallel()

		req := e.setStruct(clientTestBRequest(t), &args{Int: 5, String: "foo"})

		require.Equal(t, "5", e.get(req, "int"))
		require.Equal(t, "foo", e.get(req, "string"))
	})

	t.Run("del "+e.noun+"s", func(t *testing.T) {
		t.Parallel()

		req := e.setMany(clientTestBRequest(t), map[string]string{"foo": "bar", "bar": "foo"})
		require.Equal(t, "bar", e.get(req, "foo"))
		require.Equal(t, "foo", e.get(req, "bar"))

		e.del(req, "foo")
		require.Empty(t, e.get(req, "foo"))
		require.Equal(t, "foo", e.get(req, "bar"))
	})
}

// The kinds of entry a request holds.
var (
	clientTestBHeaders = clientTestBMultiValue{
		noun:    "header",
		add:     (*Request).AddHeader,
		set:     (*Request).SetHeader,
		addMany: (*Request).AddHeaders,
		setMany: (*Request).SetHeaders,
		get:     (*Request).Header,
	}

	clientTestBParams = clientTestBMultiValue{
		noun:      "param",
		add:       (*Request).AddParam,
		set:       (*Request).SetParam,
		addMany:   (*Request).AddParams,
		setMany:   (*Request).SetParams,
		setStruct: (*Request).SetParamsWithStruct,
		del:       (*Request).DelParams,
		get:       (*Request).Param,
	}

	clientTestBFormData = clientTestBMultiValue{
		noun:      "form field",
		add:       (*Request).AddFormData,
		set:       (*Request).SetFormData,
		addMany:   (*Request).AddFormDataWithMap,
		setMany:   (*Request).SetFormDataWithMap,
		setStruct: (*Request).SetFormDataWithStruct,
		del:       (*Request).DelFormData,
		get:       (*Request).FormData,
	}

	clientTestBCookies = clientTestBSingleValue{
		noun:      "cookie",
		get:       (*Request).Cookie,
		set:       (*Request).SetCookie,
		setMany:   (*Request).SetCookies,
		setStruct: (*Request).SetCookiesWithStruct,
		del:       (*Request).DelCookies,
	}

	clientTestBPathParams = clientTestBSingleValue{
		noun:      "path param",
		get:       (*Request).PathParam,
		set:       (*Request).SetPathParam,
		setMany:   (*Request).SetPathParams,
		setStruct: (*Request).SetPathParamsWithStruct,
		del:       (*Request).DelPathParams,
	}
)

// Test_Request_Setters sets each string field of a request to its values in turn:
// every call must replace the value the previous one set.
func Test_Request_Setters(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		set    func(r *Request, value string) *Request
		get    func(r *Request) string
		name   string
		values []string
	}{
		{name: "method", set: (*Request).SetMethod, get: (*Request).Method, values: []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "HEAD", "TRACE", "CUSTOM"}},
		{name: "URL", set: (*Request).SetURL, get: (*Request).URL, values: []string{"http://example.com/normal", "https://example.com/normal"}},
		{name: "user agent", set: (*Request).SetUserAgent, get: (*Request).UserAgent, values: []string{"fiber", "foo"}},
		{name: "referer", set: (*Request).SetReferer, get: (*Request).Referer, values: []string{"http://example.com", "https://example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := clientTestBRequest(t)
			for _, value := range tc.values {
				req = tc.set(req, value)
				require.Equal(t, value, tc.get(req), "after setting %q", value)
			}
		})
	}
}

func Test_Request_Client(t *testing.T) {
	t.Parallel()

	client := New()
	req := AcquireRequest()

	req.SetClient(client)
	require.Equal(t, client, req.Client())
}

func Test_Request_Context(t *testing.T) {
	t.Parallel()

	req := AcquireRequest()
	ctx := req.Context()
	type ctxKey struct{}
	var key ctxKey = struct{}{}

	require.Nil(t, ctx.Value(key))

	ctx = context.WithValue(ctx, key, "string")
	req.SetContext(ctx)
	ctx = req.Context()

	v, ok := ctx.Value(key).(string)
	require.True(t, ok)
	require.Equal(t, "string", v)
}

func Test_Request_Header(t *testing.T) {
	t.Parallel()

	clientTestBHeaders.run(t)
}

func Test_Request_Headers(t *testing.T) {
	t.Parallel()

	req := AcquireRequest()
	req.AddHeaders(map[string][]string{"foo": {"bar", "fiber"}, "bar": {"foo"}})

	headers := make(map[string][]string)
	for k, v := range req.Headers() {
		headers[k] = append(headers[k], v...)
	}

	require.Equal(t, []string{"bar", "fiber"}, headers["Foo"])
	require.Equal(t, []string{"foo"}, headers["Bar"])

	require.Len(t, headers, 2)
}

func Benchmark_Request_Headers(b *testing.B) {
	req := AcquireRequest()
	req.AddHeaders(map[string][]string{"foo": {"bar", "fiber"}, "bar": {"foo"}})

	b.ReportAllocs()

	for b.Loop() {
		for k, v := range req.Headers() {
			_ = k
			_ = v
		}
	}
}

func Test_Request_QueryParam(t *testing.T) {
	t.Parallel()

	clientTestBParams.run(t)
	clientTestBParams.runStruct(t)
	clientTestBParams.runDel(t)
}

var keysTestCases = []struct {
	name string
	del  string
	add  []string
	want []string
}{
	{name: "empty", want: []string{}},
	{name: "repeated key", add: []string{"foo", "foo", "bar", "foo"}, want: []string{"foo", "bar"}},
	{name: "interleaved keys", add: []string{"a", "b", "a", "c", "b"}, want: []string{"a", "b", "c"}},
	{name: "after Del", add: []string{"a", "b", "a", "c"}, del: "a", want: []string{"b", "c"}},
}

// clientTestBKeyed is what the Keys tests use of QueryParam and FormData, the
// types that wrap fasthttp.Args and list each of its keys once.
type clientTestBKeyed interface {
	Add(key, val string)
	Del(key string)
	Keys() []string
}

func Test_QueryParam_And_FormData_Keys(t *testing.T) {
	t.Parallel()

	for _, kind := range []struct {
		wrap func(args *fasthttp.Args) clientTestBKeyed
		name string
	}{
		{name: "QueryParam", wrap: func(args *fasthttp.Args) clientTestBKeyed { return &QueryParam{Args: args} }},
		{name: "FormData", wrap: func(args *fasthttp.Args) clientTestBKeyed { return &FormData{Args: args} }},
	} {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()

			for _, tc := range keysTestCases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()

					args := fasthttp.AcquireArgs()
					defer fasthttp.ReleaseArgs(args)
					p := kind.wrap(args)
					for _, k := range tc.add {
						p.Add(k, "v")
					}
					if tc.del != "" {
						p.Del(tc.del)
					}
					require.Equal(t, tc.want, p.Keys())
				})
			}
		})
	}
}

func Benchmark_QueryParam_Keys(b *testing.B) {
	p := &QueryParam{Args: fasthttp.AcquireArgs()}
	defer fasthttp.ReleaseArgs(p.Args)
	for i := range 1000 {
		p.Add("k"+strconv.Itoa(i), "v")
	}

	b.ReportAllocs()

	for b.Loop() {
		_ = p.Keys()
	}
}

func Test_Request_Params(t *testing.T) {
	t.Parallel()

	clientTestBParams.runIter(t, (*Request).Params)
}

func Benchmark_Request_Params(b *testing.B) {
	req := AcquireRequest()
	req.AddParams(map[string][]string{"foo": {"bar", "fiber"}, "bar": {"foo"}})

	b.ReportAllocs()

	for b.Loop() {
		for k, v := range req.Params() {
			_ = k
			_ = v
		}
	}
}

func Test_Request_Cookie(t *testing.T) {
	t.Parallel()

	clientTestBCookies.run(t)
}

func Test_Request_Cookies(t *testing.T) {
	t.Parallel()

	req := AcquireRequest()
	req.SetCookies(map[string]string{"foo": "bar", "bar": "foo"})

	cookies := maps.Collect(req.Cookies())

	require.Equal(t, "bar", cookies["foo"])
	require.Equal(t, "foo", cookies["bar"])

	require.NotPanics(t, func() {
		for _, v := range req.Cookies() {
			if v == "bar" {
				break
			}
		}
	})

	require.Len(t, cookies, 2)
}

func Benchmark_Request_Cookies(b *testing.B) {
	req := AcquireRequest()
	req.SetCookies(map[string]string{"foo": "bar", "bar": "foo"})

	b.ReportAllocs()

	for b.Loop() {
		for k, v := range req.Cookies() {
			_ = k
			_ = v
		}
	}
}

func Test_Request_PathParam(t *testing.T) {
	t.Parallel()

	clientTestBPathParams.run(t)

	t.Run("clear path params", func(t *testing.T) {
		t.Parallel()

		req := clientTestBRequest(t).SetPathParams(map[string]string{"foo": "bar", "bar": "foo"})
		require.Equal(t, "bar", req.PathParam("foo"))
		require.Equal(t, "foo", req.PathParam("bar"))

		req.ResetPathParams()
		require.Empty(t, req.PathParam("foo"))
		require.Empty(t, req.PathParam("bar"))
	})
}

func Test_Request_PathParams(t *testing.T) {
	t.Parallel()

	req := AcquireRequest()
	req.SetPathParams(map[string]string{"foo": "bar", "bar": "foo"})

	pathParams := maps.Collect(req.PathParams())

	require.Equal(t, "bar", pathParams["foo"])
	require.Equal(t, "foo", pathParams["bar"])

	require.Len(t, pathParams, 2)

	require.NotPanics(t, func() {
		for _, v := range req.PathParams() {
			if v == "bar" {
				break
			}
		}
	})
}

func Benchmark_Request_PathParams(b *testing.B) {
	req := AcquireRequest()
	req.SetPathParams(map[string]string{"foo": "bar", "bar": "foo"})

	b.ReportAllocs()

	for b.Loop() {
		for k, v := range req.PathParams() {
			_ = k
			_ = v
		}
	}
}

func Test_Request_FormData(t *testing.T) {
	t.Parallel()

	clientTestBFormData.run(t)
	clientTestBFormData.runStruct(t)
	clientTestBFormData.runDel(t)
}

func Test_Request_File(t *testing.T) {
	t.Parallel()

	t.Run("add file", func(t *testing.T) {
		t.Parallel()
		req := AcquireRequest().AddFile("../.github/index.html").AddFiles(AcquireFile(SetFileName("tmp.txt")))

		require.Equal(t, "../.github/index.html", req.File("index.html").path)
		require.Equal(t, "../.github/index.html", req.FileByPath("../.github/index.html").path)
		require.Equal(t, "tmp.txt", req.File("tmp.txt").name)
		require.Nil(t, req.File("tmp2.txt"))
		require.Nil(t, req.FileByPath("tmp2.txt"))
	})

	t.Run("add file by reader", func(t *testing.T) {
		t.Parallel()
		req := AcquireRequest().AddFileWithReader("tmp.txt", io.NopCloser(strings.NewReader("world")))

		require.Equal(t, "tmp.txt", req.File("tmp.txt").name)

		content, err := io.ReadAll(req.File("tmp.txt").reader)
		require.NoError(t, err)
		require.Equal(t, "world", string(content))
	})

	t.Run("add files", func(t *testing.T) {
		t.Parallel()
		req := AcquireRequest().AddFiles(AcquireFile(SetFileName("tmp.txt")), AcquireFile(SetFileName("foo.txt")))

		require.Equal(t, "tmp.txt", req.File("tmp.txt").name)
		require.Equal(t, "foo.txt", req.File("foo.txt").name)
	})
}

func Test_Request_Files(t *testing.T) {
	t.Parallel()

	req := AcquireRequest()
	req.AddFile("../.github/index.html")
	req.AddFiles(AcquireFile(SetFileName("tmp.txt")))

	files := req.Files()

	require.Equal(t, "../.github/index.html", files[0].path)
	require.Nil(t, files[0].reader)

	require.Equal(t, "tmp.txt", files[1].name)
	require.Nil(t, files[1].reader)

	require.Len(t, files, 2)
}

func Benchmark_Request_Files(b *testing.B) {
	req := AcquireRequest()
	req.AddFile("../.github/index.html")
	req.AddFiles(AcquireFile(SetFileName("tmp.txt")))

	b.ReportAllocs()

	for b.Loop() {
		for k, v := range req.Files() {
			_ = k
			_ = v
		}
	}
}

func Test_Request_Timeout(t *testing.T) {
	t.Parallel()

	req := AcquireRequest().SetTimeout(5 * time.Second)

	require.Equal(t, 5*time.Second, req.Timeout())
}

func Test_Request_Invalid_URL(t *testing.T) {
	t.Parallel()

	resp, err := AcquireRequest().Get("http://example.com\r\n\r\nGET /\r\n\r\n")

	require.Equal(t, ErrURLFormat, err)
	require.Equal(t, (*Response)(nil), resp)
}

func Test_Request_Unsupported_Protocol(t *testing.T) {
	t.Parallel()

	resp, err := AcquireRequest().Get("ftp://example.com")
	require.Equal(t, ErrURLFormat, err)
	require.Equal(t, (*Response)(nil), resp)
}

// Test_Request_Methods_With_Server sends five requests through each method that
// sends a request (Get, Post, Head, Put, Delete, Options, Patch, Query and Send)
// to a route registered for the HTTP method it uses, and checks the status and
// body of every response.
func Test_Request_Methods_With_Server(t *testing.T) {
	t.Parallel()

	const target = "http://example.com"

	// What the routes answer with.
	hostname := func(c fiber.Ctx) error { return c.SendString(c.Hostname()) }
	form := func(c fiber.Ctx) error { return c.SendString(c.FormValue("foo")) }
	created := func(c fiber.Ctx) error {
		return c.Status(fiber.StatusCreated).SendString(c.FormValue("foo"))
	}
	echo := func(c fiber.Ctx) error { return c.Send(c.Body()) }
	reply := func(status int, body string) fiber.Handler {
		return func(c fiber.Ctx) error { return c.Status(status).SendString(body) }
	}

	// What the requests carry and how they are sent.
	withForm := func(r *Request) { r.SetFormData("foo", "bar") }
	withRawBody := func(r *Request) { r.SetRawBody([]byte("query body")) }
	send := func(r *Request, url string) (*Response, error) {
		return r.SetURL(url).SetMethod(fiber.MethodPost).Send()
	}

	tests := []struct {
		register func(app *fiber.App, path string, handler any, handlers ...any) fiber.Router
		handler  fiber.Handler
		call     func(r *Request, url string) (*Response, error)
		setup    func(r *Request)
		name     string
		body     string
		status   int
	}{
		{name: "get", register: (*fiber.App).Get, handler: hostname, call: (*Request).Get, status: fiber.StatusOK, body: "example.com"},
		{name: "post", register: (*fiber.App).Post, handler: created, setup: withForm, call: (*Request).Post, status: fiber.StatusCreated, body: "bar"},
		// A response to HEAD carries no body, whatever the handler sends.
		{name: "head", register: (*fiber.App).Head, handler: hostname, call: (*Request).Head, status: fiber.StatusOK},
		{name: "put", register: (*fiber.App).Put, handler: form, setup: withForm, call: (*Request).Put, status: fiber.StatusOK, body: "bar"},
		// A 204 response carries no body, whatever the handler sends.
		{name: "delete", register: (*fiber.App).Delete, handler: reply(fiber.StatusNoContent, "deleted"), call: (*Request).Delete, status: fiber.StatusNoContent},
		{name: "options", register: (*fiber.App).Options, handler: reply(fiber.StatusOK, "options"), call: (*Request).Options, status: fiber.StatusOK, body: "options"},
		{name: "send", register: (*fiber.App).Post, handler: reply(fiber.StatusOK, "post"), call: send, status: fiber.StatusOK, body: "post"},
		{name: "patch", register: (*fiber.App).Patch, handler: form, setup: withForm, call: (*Request).Patch, status: fiber.StatusOK, body: "bar"},
		{name: "query", register: (*fiber.App).Query, handler: echo, setup: withRawBody, call: (*Request).Query, status: fiber.StatusOK, body: "query body"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := clientTestAClient(t, func(app *fiber.App) { tc.register(app, "/", tc.handler) })

			for range 5 {
				req := AcquireRequest().SetClient(client)
				if tc.setup != nil {
					tc.setup(req)
				}

				resp, err := tc.call(req, target)
				require.NoError(t, err)
				require.Equal(t, tc.status, resp.StatusCode())
				require.Equal(t, tc.body, resp.String())
				resp.Close()
			}
		})
	}
}

func Test_Request_Header_With_Server(t *testing.T) {
	t.Parallel()
	handler := func(c fiber.Ctx) error {
		for key, value := range c.Request().Header.All() {
			if k := string(key); k == "K1" || k == "K2" {
				_, err := c.Write(key)
				require.NoError(t, err)
				_, err = c.Write(value)
				require.NoError(t, err)
			}
		}
		return nil
	}

	wrapAgent := func(r *Request) {
		r.SetHeader("k1", "v1").AddHeader("k1", "v11").AddHeaders(map[string][]string{"k1": {"v22", "v33"}}).
			SetHeaders(map[string]string{"k2": "v2"}).
			AddHeader("k2", "v22")
	}

	testRequest(t, handler, wrapAgent, "K1v1K1v11K1v22K1v33K2v2K2v22")
}

func Test_Request_UserAgent_With_Server(t *testing.T) {
	t.Parallel()

	handler := func(c fiber.Ctx) error { return c.Send(c.Request().Header.UserAgent()) }

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		testRequest(t, handler, func(_ *Request) {}, defaultUserAgent, 5)
	})

	t.Run("custom", func(t *testing.T) {
		t.Parallel()
		testRequest(t, handler, func(agent *Request) { agent.SetUserAgent("ua") }, "ua", 5)
	})
}

func Test_Request_Cookie_With_Server(t *testing.T) {
	t.Parallel()
	handler := func(c fiber.Ctx) error {
		return c.SendString(c.Cookies("k1") + c.Cookies("k2") + c.Cookies("k3") + c.Cookies("k4"))
	}

	wrapAgent := func(req *Request) {
		req.SetCookie("k1", "v1").SetCookies(map[string]string{"k2": "v2", "k3": "v3", "k4": "v4"}).DelCookies("k4")
	}

	testRequest(t, handler, wrapAgent, "v1v2v3")
}

func Test_Request_Referer_With_Server(t *testing.T) {
	t.Parallel()
	handler := func(c fiber.Ctx) error { return c.Send(c.Request().Header.Referer()) }

	wrapAgent := func(req *Request) { req.SetReferer("http://referer.com") }

	testRequest(t, handler, wrapAgent, "http://referer.com")
}

func Test_Request_QueryString_With_Server(t *testing.T) {
	t.Parallel()
	handler := func(c fiber.Ctx) error { return c.Send(c.Request().URI().QueryString()) }

	wrapAgent := func(req *Request) {
		req.SetParam("foo", "bar").SetParams(map[string]string{"bar": "baz"})
	}

	testRequest(t, handler, wrapAgent, "foo=bar&bar=baz")
}

func checkFormFile(t *testing.T, fh *multipart.FileHeader, filename string) {
	t.Helper()

	basename := filepath.Base(filename)
	require.Equal(t, fh.Filename, basename)

	b1, err := os.ReadFile(filepath.Clean(filename))
	require.NoError(t, err)

	b2 := make([]byte, fh.Size)
	f, err := fh.Open()
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	_, err = f.Read(b2)
	require.NoError(t, err)
	require.Equal(t, b1, b2)
}

func Test_Request_Body_With_Server(t *testing.T) {
	t.Parallel()

	// The tags of both encodings: each reads only its own.
	type args struct {
		Content string `xml:"content" cbor:"content"`
	}

	for _, tc := range []struct {
		set         func(r *Request)
		name        string
		contentType string
		want        string
	}{
		{name: "json body", contentType: "application/json", set: func(r *Request) { r.SetJSON(map[string]string{"success": "hello"}) }, want: "{\"success\":\"hello\"}"},
		{name: "xml body", contentType: "application/xml", set: func(r *Request) { r.SetXML(args{Content: "hello"}) }, want: "<args><content>hello</content></args>"},
		{name: "cbor body", contentType: "application/cbor", set: func(r *Request) { r.SetCBOR(args{Content: "hello"}) }, want: "\xa1gcontentehello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testRequest(
				t,
				func(c fiber.Ctx) error {
					require.Equal(t, tc.contentType, string(c.Request().Header.ContentType()))
					return c.SendString(string(c.Request().Body()))
				},
				tc.set,
				tc.want,
			)
		})
	}

	t.Run("formdata", func(t *testing.T) {
		t.Parallel()
		testRequest(t,
			func(c fiber.Ctx) error {
				require.Equal(t, fiber.MIMEApplicationForm, string(c.Request().Header.ContentType()))
				return c.Send([]byte("foo=" + c.FormValue("foo") + "&bar=" + c.FormValue("bar") + "&fiber=" + c.FormValue("fiber")))
			},
			func(agent *Request) {
				agent.SetFormData("foo", "bar").SetFormDataWithMap(map[string]string{"bar": "baz", "fiber": "fast"})
			},
			"foo=bar&bar=baz&fiber=fast")
	})

	t.Run("multipart form", func(t *testing.T) {
		t.Parallel()

		client := clientTestAClient(t, func(app *fiber.App) {
			app.Post("/", func(c fiber.Ctx) error {
				require.Equal(t, "multipart/form-data; boundary=myBoundary", c.Get(fiber.HeaderContentType))

				mf, err := c.MultipartForm()
				require.NoError(t, err)
				require.Equal(t, "bar", mf.Value["foo"][0])

				return c.Send(c.Request().Body())
			})
		})

		req := AcquireRequest().SetClient(client).SetBoundary("myBoundary").SetFormData("foo", "bar").
			AddFiles(AcquireFile(
				SetFileName("hello.txt"),
				SetFileFieldName("foo"),
				SetFileReader(io.NopCloser(strings.NewReader("world"))),
			))

		require.Equal(t, "myBoundary", req.Boundary())

		resp, err := req.Post("http://example.com")
		require.NoError(t, err)
		require.Equal(t, fiber.StatusOK, resp.StatusCode())

		form, err := multipart.NewReader(bytes.NewReader(resp.Body()), "myBoundary").ReadForm(1024 * 1024)
		require.NoError(t, err)
		require.Equal(t, "bar", form.Value["foo"][0])
		resp.Close()
	})

	t.Run("multipart form send file", func(t *testing.T) {
		t.Parallel()

		client := clientTestAClient(t, func(app *fiber.App) {
			app.Post("/", func(c fiber.Ctx) error {
				require.Equal(t, "multipart/form-data; boundary=myBoundary", c.Get(fiber.HeaderContentType))

				fh1, err := c.FormFile("field1")
				require.NoError(t, err)
				require.Equal(t, "name", fh1.Filename)
				buf := make([]byte, fh1.Size)
				f, err := fh1.Open()
				require.NoError(t, err)
				defer func() { require.NoError(t, f.Close()) }()
				_, err = f.Read(buf)
				require.NoError(t, err)
				require.Equal(t, "form file", string(buf))

				fh2, err := c.FormFile("file2")
				require.NoError(t, err)
				checkFormFile(t, fh2, "../.github/testdata/index.html")

				fh3, err := c.FormFile("file3")
				require.NoError(t, err)
				checkFormFile(t, fh3, "../.github/testdata/index.tmpl")

				return c.SendString("multipart form files")
			})
		})

		for range 5 {
			req := AcquireRequest().SetClient(client).
				AddFiles(
					AcquireFile(
						SetFileFieldName("field1"),
						SetFileName("name"),
						SetFileReader(io.NopCloser(bytes.NewReader([]byte("form file")))),
					),
				).
				AddFile("../.github/testdata/index.html").
				AddFile("../.github/testdata/index.tmpl").
				SetBoundary("myBoundary")

			resp, err := req.Post("http://example.com")
			require.NoError(t, err)
			require.Equal(t, "multipart form files", resp.String())

			resp.Close()
		}
	})

	t.Run("multipart random boundary", func(t *testing.T) {
		t.Parallel()

		client := clientTestAClient(t, func(app *fiber.App) {
			app.Post("/", func(c fiber.Ctx) error {
				reg := regexp.MustCompile(`multipart/form-data; boundary=[\-\w]{33}`)
				require.True(t, reg.MatchString(c.Get(fiber.HeaderContentType)))

				return c.Send(c.Request().Body())
			})
		})

		req := AcquireRequest().SetClient(client).SetFormData("foo", "bar").
			AddFiles(AcquireFile(
				SetFileName("hello.txt"),
				SetFileFieldName("foo"),
				SetFileReader(io.NopCloser(strings.NewReader("world"))),
			))

		resp, err := req.Post("http://example.com")
		require.NoError(t, err)
		require.Equal(t, fiber.StatusOK, resp.StatusCode())
	})

	t.Run("raw body", func(t *testing.T) {
		t.Parallel()
		testRequest(
			t,
			func(c fiber.Ctx) error { return c.SendString(string(c.Request().Body())) },
			func(agent *Request) { agent.SetRawBody([]byte("hello")) },
			"hello",
		)
	})
}

func Test_Request_AllFormData(t *testing.T) {
	t.Parallel()

	clientTestBFormData.runIter(t, (*Request).AllFormData)
}

func Benchmark_Request_AllFormData(b *testing.B) {
	req := AcquireRequest()
	req.AddFormDataWithMap(map[string][]string{"foo": {"bar", "fiber"}, "bar": {"foo"}})

	b.ReportAllocs()

	for b.Loop() {
		for k, v := range req.AllFormData() {
			_ = k
			_ = v
		}
	}
}

func Test_Request_Error_Body_With_Server(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		set  func(r *Request)
		want error
		name string
	}{
		{name: "json error", set: func(r *Request) { r.SetJSON(complex(1, 1)) }, want: errors.New("json: unsupported type: complex128")},
		{name: "xml error", set: func(r *Request) { r.SetXML(complex(1, 1)) }, want: errors.New("xml: unsupported type: complex128")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testRequestFail(t, func(c fiber.Ctx) error { return c.SendString("") }, tc.set, tc.want)
		})
	}

	t.Run("form body with invalid boundary", func(t *testing.T) {
		t.Parallel()

		_, err := AcquireRequest().SetBoundary("*").
			AddFileWithReader("t.txt", io.NopCloser(strings.NewReader("world"))).
			Get("http://example.com")
		require.Equal(t, "set boundary error: mime: invalid boundary character", err.Error())
	})

	t.Run("open non exist file", func(t *testing.T) {
		t.Parallel()

		_, err := AcquireRequest().AddFile("non-exist-file!").Get("http://example.com")
		require.Contains(t, err.Error(), "open non-exist-file!")
	})
}

func Test_Request_Timeout_With_Server(t *testing.T) {
	t.Parallel()

	client := clientTestAClient(t, func(app *fiber.App) {
		app.Get("/", func(c fiber.Ctx) error {
			time.Sleep(time.Millisecond * 200)
			return c.SendString("timeout")
		})
	})

	_, err := AcquireRequest().SetClient(client).SetTimeout(50 * time.Millisecond).Get("http://example.com")

	require.Equal(t, ErrTimeoutOrCancel, err)
}

func Test_Request_MaxRedirects(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	app := fiber.New()

	app.Get("/", func(c fiber.Ctx) error {
		if c.Request().URI().QueryArgs().Has("foo") {
			return c.Redirect().To("/foo")
		}
		return c.Redirect().To("/")
	})
	app.Get("/foo", func(c fiber.Ctx) error { return c.SendString("redirect") })

	go func() { assert.NoError(t, app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})) }()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		client := New().SetDial(func(_ string) (net.Conn, error) { return ln.Dial() })

		resp, err := AcquireRequest().SetClient(client).SetMaxRedirects(1).Get("http://example.com?foo")
		body := resp.String()
		code := resp.StatusCode()

		require.Equal(t, 200, code)
		require.Equal(t, "redirect", body)
		require.NoError(t, err)

		resp.Close()
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()

		client := New().SetDial(func(_ string) (net.Conn, error) { return ln.Dial() })

		resp, err := AcquireRequest().SetClient(client).SetMaxRedirects(1).Get("http://example.com")

		require.Nil(t, resp)
		require.ErrorIs(t, err, fasthttp.ErrTooManyRedirects)
	})

	t.Run("MaxRedirects", func(t *testing.T) {
		t.Parallel()

		client := New().SetDial(func(_ string) (net.Conn, error) { return ln.Dial() })

		req := AcquireRequest().SetClient(client).SetMaxRedirects(3)

		require.Equal(t, 3, req.MaxRedirects())
	})
}

func Test_Request_Query_Redirect(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	app := fiber.New()
	// 308 preserves method and body, so QUERY must follow it and keep its body.
	app.Query("/start", func(c fiber.Ctx) error {
		return c.Redirect().Status(fiber.StatusPermanentRedirect).To("/end")
	})
	app.Query("/end", func(c fiber.Ctx) error { return c.Send(c.Body()) })

	go func() { assert.NoError(t, app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})) }()

	client := New().SetDial(func(_ string) (net.Conn, error) { return ln.Dial() })

	resp, err := AcquireRequest().SetClient(client).SetMaxRedirects(1).SetRawBody([]byte("query body")).
		Query("http://example.com/start")

	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode())
	require.Equal(t, "query body", resp.String())

	resp.Close()
}

func Test_SetValWithStruct(t *testing.T) {
	t.Parallel()

	// test SetValWithStruct via QueryParam struct.
	type args struct {
		TString   string
		TSlice    []string
		TIntSlice []int `param:"int_slice"`
		unexport  int
		TInt      int
		TUint     uint
		TFloat    float64
		TComplex  complex128
		TBool     bool
	}

	t.Run("the struct should be applied", func(t *testing.T) {
		t.Parallel()
		p := &QueryParam{Args: fasthttp.AcquireArgs()}

		SetValWithStruct(p, "param", args{
			unexport:  5,
			TInt:      5,
			TUint:     5,
			TString:   "string",
			TFloat:    3.1,
			TComplex:  3 + 4i,
			TBool:     false,
			TSlice:    []string{"foo", "bar"},
			TIntSlice: []int{0, 1, 2},
		})

		require.Empty(t, string(p.Peek("unexport")))
		require.Equal(t, []byte("5"), p.Peek("TInt"))
		require.Equal(t, []byte("5"), p.Peek("TUint"))
		require.Equal(t, []byte("string"), p.Peek("TString"))
		require.Equal(t, []byte("3.1"), p.Peek("TFloat"))
		require.Equal(t, []byte("(3+4i)"), p.Peek("TComplex"))
		require.Equal(t, []byte("false"), p.Peek("TBool"))
		require.Contains(t, p.PeekMulti("TSlice"), []byte("foo"))
		require.Contains(t, p.PeekMulti("TSlice"), []byte("bar"))
		require.Contains(t, p.PeekMulti("int_slice"), []byte("0"))
		require.Contains(t, p.PeekMulti("int_slice"), []byte("1"))
		require.Contains(t, p.PeekMulti("int_slice"), []byte("2"))
	})

	t.Run("the pointer of a struct should be applied", func(t *testing.T) {
		t.Parallel()
		p := &QueryParam{Args: fasthttp.AcquireArgs()}

		SetValWithStruct(p, "param", &args{
			TInt:      5,
			TString:   "string",
			TFloat:    3.1,
			TBool:     true,
			TSlice:    []string{"foo", "bar"},
			TIntSlice: []int{1, 2},
		})

		require.Equal(t, []byte("5"), p.Peek("TInt"))
		require.Equal(t, []byte("string"), p.Peek("TString"))
		require.Equal(t, []byte("3.1"), p.Peek("TFloat"))
		require.Equal(t, "true", string(p.Peek("TBool")))
		require.Contains(t, p.PeekMulti("TSlice"), []byte("foo"))
		require.Contains(t, p.PeekMulti("TSlice"), []byte("bar"))
		require.Contains(t, p.PeekMulti("int_slice"), []byte("1"))
		require.Contains(t, p.PeekMulti("int_slice"), []byte("2"))
	})

	t.Run("error type should ignore", func(t *testing.T) {
		t.Parallel()
		p := &QueryParam{Args: fasthttp.AcquireArgs()}
		SetValWithStruct(p, "param", 5)
		require.Equal(t, 0, p.Len())
	})
}

func Benchmark_SetValWithStruct(b *testing.B) {
	// test SetValWithStruct via QueryParam struct.
	type args struct {
		TString   string
		TSlice    []string
		TIntSlice []int `param:"int_slice"`
		unexport  int
		TInt      int
		TUint     uint
		TFloat    float64
		TComplex  complex128
		TBool     bool
	}

	b.Run("the struct should be applied", func(b *testing.B) {
		p := &QueryParam{Args: fasthttp.AcquireArgs()}

		b.ReportAllocs()

		for b.Loop() {
			SetValWithStruct(p, "param", args{
				unexport:  5,
				TInt:      5,
				TUint:     5,
				TString:   "string",
				TFloat:    3.1,
				TComplex:  3 + 4i,
				TBool:     false,
				TSlice:    []string{"foo", "bar"},
				TIntSlice: []int{0, 1, 2},
			})
		}

		require.Empty(b, string(p.Peek("unexport")))
		require.Equal(b, []byte("5"), p.Peek("TInt"))
		require.Equal(b, []byte("5"), p.Peek("TUint"))
		require.Equal(b, []byte("string"), p.Peek("TString"))
		require.Equal(b, []byte("3.1"), p.Peek("TFloat"))
		require.Equal(b, []byte("(3+4i)"), p.Peek("TComplex"))
		require.Equal(b, []byte("false"), p.Peek("TBool"))
		require.True(b, func() bool {
			for _, v := range p.PeekMulti("TSlice") {
				if string(v) == "foo" {
					return true
				}
			}
			return false
		}())

		require.True(b, func() bool {
			for _, v := range p.PeekMulti("TSlice") {
				if string(v) == "bar" {
					return true
				}
			}
			return false
		}())

		require.True(b, func() bool {
			for _, v := range p.PeekMulti("int_slice") {
				if string(v) == "0" {
					return true
				}
			}
			return false
		}())

		require.True(b, func() bool {
			for _, v := range p.PeekMulti("int_slice") {
				if string(v) == "1" {
					return true
				}
			}
			return false
		}())

		require.True(b, func() bool {
			for _, v := range p.PeekMulti("int_slice") {
				if string(v) == "2" {
					return true
				}
			}
			return false
		}())
	})

	b.Run("the pointer of a struct should be applied", func(b *testing.B) {
		p := &QueryParam{Args: fasthttp.AcquireArgs()}

		b.ReportAllocs()

		for b.Loop() {
			SetValWithStruct(p, "param", &args{
				TInt:      5,
				TString:   "string",
				TFloat:    3.1,
				TBool:     true,
				TSlice:    []string{"foo", "bar"},
				TIntSlice: []int{1, 2},
			})
		}

		require.Equal(b, []byte("5"), p.Peek("TInt"))
		require.Equal(b, []byte("string"), p.Peek("TString"))
		require.Equal(b, []byte("3.1"), p.Peek("TFloat"))
		require.Equal(b, "true", string(p.Peek("TBool")))
		require.True(b, func() bool {
			for _, v := range p.PeekMulti("TSlice") {
				if string(v) == "foo" {
					return true
				}
			}
			return false
		}())

		require.True(b, func() bool {
			for _, v := range p.PeekMulti("TSlice") {
				if string(v) == "bar" {
					return true
				}
			}
			return false
		}())

		require.True(b, func() bool {
			for _, v := range p.PeekMulti("int_slice") {
				if string(v) == "1" {
					return true
				}
			}
			return false
		}())

		require.True(b, func() bool {
			for _, v := range p.PeekMulti("int_slice") {
				if string(v) == "2" {
					return true
				}
			}
			return false
		}())
	})

	b.Run("error type should ignore", func(b *testing.B) {
		p := &QueryParam{Args: fasthttp.AcquireArgs()}

		b.ReportAllocs()

		for b.Loop() {
			SetValWithStruct(p, "param", 5)
		}

		require.Equal(b, 0, p.Len())
	})
}
