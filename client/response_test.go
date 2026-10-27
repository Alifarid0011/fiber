package client

import (
	"bytes"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofiber/fiber/v3/internal/tlstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

// clientTestBResponse returns a pooled response that is released when the test ends.
func clientTestBResponse(t *testing.T) *Response {
	t.Helper()

	resp := AcquireResponse()
	t.Cleanup(func() { ReleaseResponse(resp) })

	return resp
}

// clientTestBGet serves routes from a fresh test server and returns the response to
// a GET for url, sent through a client that dials that server. The response is
// closed, and then the server stopped, when the test ends.
func clientTestBGet(t *testing.T, routes func(app *fiber.App), url string) *Response {
	t.Helper()

	resp, err := AcquireRequest().SetClient(clientTestAClient(t, routes)).Get(url)
	require.NoError(t, err)
	t.Cleanup(resp.Close)

	return resp
}

// clientTestBClientGet returns the response to client.Get(url), unlike
// [clientTestBGet] sent as a request the client owns. The response is closed when
// the test ends.
func clientTestBClientGet(t *testing.T, client *Client, url string) *Response {
	t.Helper()

	resp, err := client.Get(url)
	require.NoError(t, err)
	t.Cleanup(resp.Close)

	return resp
}

// Test_Response_Status checks the status text and the status code of a successful
// and of a failing response. Every check has its own server.
func Test_Response_Status(t *testing.T) {
	t.Parallel()
	routes := func(app *fiber.App) {
		app.Get("/", func(c fiber.Ctx) error { return c.SendString("foo") })
		app.Get("/fail", func(c fiber.Ctx) error { return c.SendStatus(407) })
	}

	for _, tc := range []struct {
		name   string
		url    string
		status string
		code   int
	}{
		{name: "success", url: "http://example", status: "OK", code: 200},
		{name: "fail", url: "http://example/fail", status: "Proxy Authentication Required", code: 407},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Run("status", func(t *testing.T) {
				t.Parallel()
				resp := clientTestBGet(t, routes, tc.url)
				require.Equal(t, tc.status, resp.Status())
			})

			t.Run("status code", func(t *testing.T) {
				t.Parallel()
				resp := clientTestBGet(t, routes, tc.url)
				require.Equal(t, tc.code, resp.StatusCode())
			})
		})
	}
}

func Test_Response_CloseUsesSnapshottedRequestOwnership(t *testing.T) {
	t.Parallel()
	req := AcquireRequest()
	resp := AcquireResponse()
	resp.setRequest(req, false)

	// Simulate the caller releasing this request and a helper reusing the pooled
	// object. The stale response must not release the helper's logical request.
	req.clientOwned = true
	resp.Close()
	require.True(t, req.clientOwned)

	ReleaseRequest(req)
}

func Test_Response_Protocol(t *testing.T) {
	t.Parallel()
	t.Run("http", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBGet(t, func(app *fiber.App) {
			app.Get("/", func(c fiber.Ctx) error { return c.SendString("foo") })
		}, "http://example")

		require.Equal(t, "HTTP/1.1", resp.Protocol())
	})

	t.Run("https", func(t *testing.T) {
		t.Parallel()
		serverTLSConf, clientTLSConf, err := tlstest.GetTLSConfigs()
		require.NoError(t, err)

		ln, err := net.Listen(fiber.NetworkTCP4, "127.0.0.1:0")
		require.NoError(t, err)

		ln = tls.NewListener(ln, serverTLSConf)

		app := fiber.New()
		app.Get("/", func(c fiber.Ctx) error { return c.SendString(c.Scheme()) })

		go func() {
			assert.NoError(t, app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}))
		}()

		client := New()
		resp, err := client.SetTLSConfig(clientTLSConf).Get("https://" + ln.Addr().String())

		require.NoError(t, err)
		require.Equal(t, clientTLSConf, client.TLSConfig())
		require.Equal(t, fiber.StatusOK, resp.StatusCode())
		require.Equal(t, "https", resp.String())
		require.Equal(t, "HTTP/1.1", resp.Protocol())

		resp.Close()
	})
}

func Test_Response_Header(t *testing.T) {
	t.Parallel()
	resp := clientTestBGet(t, func(app *fiber.App) {
		app.Get("/", func(c fiber.Ctx) error {
			c.Response().Header.Add("foo", "bar")
			return c.SendString("helo world")
		})
	}, "http://example.com")

	require.Equal(t, "bar", resp.Header("foo"))
}

func Test_Response_Headers(t *testing.T) {
	t.Parallel()
	resp := clientTestBGet(t, func(app *fiber.App) {
		app.Get("/", func(c fiber.Ctx) error {
			c.Response().Header.Add("foo", "bar")
			c.Response().Header.Add("foo", "bar2")
			c.Response().Header.Add("foo2", "bar")

			return c.SendString("hello world")
		})
	}, "http://example.com")

	headers := make(map[string][]string)
	for k, v := range resp.Headers() {
		headers[k] = append(headers[k], v...)
	}

	require.Equal(t, "hello world", resp.String())

	require.Equal(t, []string{"bar", "bar2"}, headers["Foo"])
	require.Equal(t, []string{"bar"}, headers["Foo2"])

	require.Len(t, headers, 5) // Foo + Foo2 + Date + Content-Length + Content-Type
}

func Test_Response_Headers_RepeatedKey(t *testing.T) {
	t.Parallel()
	values := []string{"1", "2", "3", "4", "5", "6", "7", "8"}
	resp := clientTestBGet(t, func(app *fiber.App) {
		app.Get("/", func(c fiber.Ctx) error {
			for _, v := range values {
				c.Response().Header.Add("X-Multi", v)
			}
			return c.SendString("hello world")
		})
	}, "http://example.com")

	headers := maps.Collect(resp.Headers())

	require.Equal(t, values, headers["X-Multi"])
	require.Len(t, headers, 4) // X-Multi + Date + Content-Length + Content-Type

	seen := 0
	for range resp.Headers() {
		seen++
		if seen == 1 {
			break
		}
	}
	require.Equal(t, 1, seen)
}

func Benchmark_Headers(b *testing.B) {
	server := startTestServer(
		b,
		func(app *fiber.App) {
			app.Get("/", func(c fiber.Ctx) error {
				c.Response().Header.Add("foo", "bar")
				c.Response().Header.Add("foo", "bar2")
				c.Response().Header.Add("foo", "bar3")

				c.Response().Header.Add("foo2", "bar")
				c.Response().Header.Add("foo2", "bar2")
				c.Response().Header.Add("foo2", "bar3")

				return c.SendString("helo world")
			})
		},
	)

	client := New().SetDial(server.dial())

	resp, err := AcquireRequest().SetClient(client).Get("http://example.com")
	require.NoError(b, err)

	b.Cleanup(func() {
		resp.Close()
		server.stop()
	})

	b.ReportAllocs()

	for b.Loop() {
		for k, v := range resp.Headers() {
			_ = k
			_ = v
		}
	}
}

func Test_Response_Cookie(t *testing.T) {
	t.Parallel()
	resp := clientTestBGet(t, func(app *fiber.App) {
		app.Get("/", func(c fiber.Ctx) error {
			c.Cookie(&fiber.Cookie{Name: "foo", Value: "bar"})
			return c.SendString("helo world")
		})
	}, "http://example.com")

	require.Equal(t, "bar", string(resp.Cookies()[0].Value()))
}

func Test_Response_Body(t *testing.T) {
	t.Parallel()
	routes := func(app *fiber.App) {
		app.Get("/", func(c fiber.Ctx) error { return c.SendString("hello world") })

		app.Get("/json", func(c fiber.Ctx) error { return c.SendString("{\"status\":\"success\"}") })

		app.Get("/xml", func(c fiber.Ctx) error { return c.SendString("<status><name>success</name></status>") })

		app.Get("/cbor", func(c fiber.Ctx) error {
			type cborData struct {
				Name string `cbor:"name"`
				Age  int    `cbor:"age"`
			}

			return c.CBOR(cborData{Name: "foo", Age: 12})
		})
	}

	t.Run("raw body", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBGet(t, routes, "http://example.com")

		require.Equal(t, []byte("hello world"), resp.Body())
	})

	t.Run("string body", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBGet(t, routes, "http://example.com")

		require.Equal(t, "hello world", resp.String())
	})

	t.Run("json body", func(t *testing.T) {
		t.Parallel()
		type body struct {
			Status string `json:"status"`
		}

		resp := clientTestBGet(t, routes, "http://example.com/json")

		tmp := &body{}
		require.NoError(t, resp.JSON(tmp))
		require.Equal(t, "success", tmp.Status)
	})

	t.Run("xml body", func(t *testing.T) {
		t.Parallel()
		type body struct {
			Name   xml.Name `xml:"status"`
			Status string   `xml:"name"`
		}

		resp := clientTestBGet(t, routes, "http://example.com/xml")

		tmp := &body{}
		require.NoError(t, resp.XML(tmp))
		require.Equal(t, "success", tmp.Status)
	})

	t.Run("cbor body", func(t *testing.T) {
		t.Parallel()
		type cborData struct {
			Name string `cbor:"name"`
			Age  int    `cbor:"age"`
		}

		data := cborData{Name: "foo", Age: 12}

		resp := clientTestBGet(t, routes, "http://example.com/cbor")

		tmp := &cborData{}
		require.NoError(t, resp.CBOR(tmp))
		require.Equal(t, data, *tmp)
	})
}

func Test_Response_DecodeHelpers_ClientNilSafety(t *testing.T) {
	t.Parallel()
	type payload struct {
		Status string `json:"status" xml:"status" cbor:"status"`
	}

	type decoder struct {
		decode func(resp *Response, v any) error
		name   string
		body   string
	}

	jsonDecoder := decoder{name: "json", decode: (*Response).JSON, body: `{"status":"success"}`}
	xmlDecoder := decoder{name: "xml", decode: (*Response).XML, body: `<payload><status>success</status></payload>`}

	t.Run("client nil returns exported error without panic", func(t *testing.T) {
		t.Parallel()

		// The body is not valid CBOR, so ErrClientNil also shows that a nil client is
		// reported before the body is decoded.
		cborDecoder := decoder{name: "cbor", decode: (*Response).CBOR, body: "not-cbor"}

		for _, tc := range []decoder{jsonDecoder, xmlDecoder, cborDecoder} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				resp := clientTestBResponse(t)
				resp.RawResponse.SetBodyString(tc.body)

				decoded := payload{}
				require.NotPanics(t, func() {
					require.ErrorIs(t, tc.decode(resp, &decoded), ErrClientNil)
				})
			})
		}
	})

	t.Run("decode helpers still work with client", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []decoder{jsonDecoder, xmlDecoder} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				resp := clientTestBResponse(t)
				resp.setClient(New())
				resp.RawResponse.SetBodyString(tc.body)

				decoded := payload{}
				require.NoError(t, tc.decode(resp, &decoded))
				require.Equal(t, "success", decoded.Status)
			})
		}

		t.Run("cbor", func(t *testing.T) {
			t.Parallel()
			client := New()
			resp := clientTestBResponse(t)
			resp.setClient(client)

			body, err := client.cborMarshal(payload{Status: "success"})
			require.NoError(t, err)
			resp.RawResponse.SetBody(body)

			decoded := payload{}
			err = resp.CBOR(&decoded)
			require.NoError(t, err)
			require.Equal(t, "success", decoded.Status)
		})
	})
}

func Test_Response_Save(t *testing.T) {
	t.Parallel()
	routes := func(app *fiber.App) {
		app.Get("/json", func(c fiber.Ctx) error { return c.SendString("{\"status\":\"success\"}") })
	}

	t.Run("file path", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBGet(t, routes, "http://example.com/json")

		err := resp.Save("./test/tmp.json")
		require.NoError(t, err)
		defer func() {
			_, statErr := os.Stat("./test/tmp.json")
			require.NoError(t, statErr)

			statErr = os.RemoveAll("./test")
			require.NoError(t, statErr)
		}()

		file, err := os.Open("./test/tmp.json")
		require.NoError(t, err)
		defer func(file *os.File) {
			closeErr := file.Close()
			require.NoError(t, closeErr)
		}(file)

		data, err := io.ReadAll(file)
		require.NoError(t, err)
		require.JSONEq(t, "{\"status\":\"success\"}", string(data))
	})

	t.Run("io.Writer", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBGet(t, routes, "http://example.com/json")

		buf := &bytes.Buffer{}

		require.NoError(t, resp.Save(buf))
		require.JSONEq(t, "{\"status\":\"success\"}", buf.String())
	})

	t.Run("io.Copy error when saving to file is surfaced", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBResponse(t)

		resp.RawResponse.SetBodyStream(&errorReader{err: errors.New("copy failure")}, -1)

		target := filepath.Join(t.TempDir(), "out.txt")
		require.ErrorContains(t, resp.Save(target), "failed to write response body to file: copy failure")
	})

	t.Run("io.Copy error when saving to writer is surfaced", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBResponse(t)

		resp.RawResponse.SetBodyStream(bytes.NewBufferString("data"), len("data"))

		require.ErrorContains(t, resp.Save(&errorWriter{err: errors.New("sink closed")}), "failed to write response body to writer: sink closed")
	})

	t.Run("error type", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBGet(t, routes, "http://example.com/json")

		require.Error(t, resp.Save(nil))
	})
}

func Test_Response_BodyStream(t *testing.T) {
	t.Parallel()
	t.Run("basic streaming", func(t *testing.T) {
		t.Parallel()
		client := clientTestAClient(t, func(app *fiber.App) {
			app.Get("/stream", func(c fiber.Ctx) error {
				return c.SendStream(bytes.NewReader([]byte("streaming data")))
			})
		}).SetStreamResponseBody(true)

		resp := clientTestBClientGet(t, client, "http://example.com/stream")
		bodyStream := resp.BodyStream()
		require.NotNil(t, bodyStream)
		data, err := io.ReadAll(bodyStream)
		require.NoError(t, err)
		require.Equal(t, "streaming data", string(data))
	})

	t.Run("large response streaming", func(t *testing.T) {
		t.Parallel()
		client := clientTestAClient(t, func(app *fiber.App) {
			app.Get("/large", func(c fiber.Ctx) error {
				data := make([]byte, 1024)
				for i := range data {
					data[i] = byte('A' + i%26)
				}
				return c.SendStream(bytes.NewReader(data))
			})
		}).SetStreamResponseBody(true)

		resp := clientTestBClientGet(t, client, "http://example.com/large")
		bodyStream := resp.BodyStream()
		require.NotNil(t, bodyStream)
		buffer := make([]byte, 256)
		var totalRead []byte
		for {
			n, err := bodyStream.Read(buffer)
			if n > 0 {
				totalRead = append(totalRead, buffer[:n]...)
			}
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
		}
		require.Len(t, totalRead, 1024)
	})
}

func Test_Response_BodyStream_Fallback(t *testing.T) {
	t.Parallel()
	t.Run("non-streaming response fallback to bytes.Reader", func(t *testing.T) {
		t.Parallel()
		client := clientTestAClient(t, func(app *fiber.App) {
			app.Get("/regular", func(c fiber.Ctx) error { return c.SendString("regular response body") })
		})
		resp := clientTestBClientGet(t, client, "http://example.com/regular")
		require.False(t, resp.IsStreaming())
		bodyStream := resp.BodyStream()
		require.NotNil(t, bodyStream)
		data, err := io.ReadAll(bodyStream)
		require.NoError(t, err)
		require.Equal(t, "regular response body", string(data))
	})
}

func Test_Response_IsStreaming(t *testing.T) {
	t.Parallel()
	t.Run("streaming disabled", func(t *testing.T) {
		t.Parallel()
		client := clientTestAClient(t, func(app *fiber.App) {
			app.Get("/regular", func(c fiber.Ctx) error { return c.SendString("regular content") })
		})
		resp := clientTestBClientGet(t, client, "http://example.com/regular")
		require.False(t, resp.IsStreaming())
	})

	t.Run("bodystream always works regardless of streaming state", func(t *testing.T) {
		t.Parallel()
		server := startTestServer(t, func(app *fiber.App) {
			app.Get("/test", func(c fiber.Ctx) error { return c.SendString("test content") })
		})
		defer server.stop()

		// Test with streaming enabled
		client1 := New().SetDial(server.dial()).SetStreamResponseBody(true)
		resp1, err := client1.Get("http://example.com/test")
		require.NoError(t, err)
		defer resp1.Close()
		bodyStream1 := resp1.BodyStream()
		require.NotNil(t, bodyStream1)
		data1, err := io.ReadAll(bodyStream1)
		require.NoError(t, err)
		require.Equal(t, "test content", string(data1))

		// Test with streaming disabled
		client2 := New().SetDial(server.dial()).SetStreamResponseBody(false)
		resp2, err := client2.Get("http://example.com/test")
		require.NoError(t, err)
		defer resp2.Close()
		require.False(t, resp2.IsStreaming())
		bodyStream2 := resp2.BodyStream()
		require.NotNil(t, bodyStream2)
		data2, err := io.ReadAll(bodyStream2)
		require.NoError(t, err)
		require.Equal(t, "test content", string(data2))
	})
}

func Test_Response_Save_Streaming(t *testing.T) {
	t.Parallel()
	t.Run("save streaming response to file", func(t *testing.T) {
		t.Parallel()
		client := clientTestAClient(t, func(app *fiber.App) {
			app.Get("/stream", func(c fiber.Ctx) error {
				return c.SendStream(bytes.NewReader([]byte("streaming file content")))
			})
		}).SetStreamResponseBody(true)

		resp := clientTestBClientGet(t, client, "http://example.com/stream")

		testFile := filepath.Join(t.TempDir(), "stream_test.txt")
		err := resp.Save(testFile)
		require.NoError(t, err)

		data, err := os.ReadFile(testFile)
		require.NoError(t, err)
		require.Equal(t, "streaming file content", string(data))
	})

	t.Run("save streaming response to io.Writer", func(t *testing.T) {
		t.Parallel()
		client := clientTestAClient(t, func(app *fiber.App) {
			app.Get("/stream", func(c fiber.Ctx) error {
				return c.SendStream(bytes.NewReader([]byte("streaming writer content")))
			})
		}).SetStreamResponseBody(true)

		resp := clientTestBClientGet(t, client, "http://example.com/stream")

		var buf bytes.Buffer
		require.NoError(t, resp.Save(&buf))
		require.Equal(t, "streaming writer content", buf.String())
	})

	t.Run("save non-streaming response to file using BodyStream", func(t *testing.T) {
		t.Parallel()
		client := clientTestAClient(t, func(app *fiber.App) {
			app.Get("/regular", func(c fiber.Ctx) error { return c.SendString("regular file content") })
		})

		resp := clientTestBClientGet(t, client, "http://example.com/regular")

		testFile := filepath.Join(t.TempDir(), "regular_test.txt")
		err := resp.Save(testFile)
		require.NoError(t, err)

		data, err := os.ReadFile(testFile)
		require.NoError(t, err)
		require.Equal(t, "regular file content", string(data))
	})

	t.Run("save to io.WriteCloser closes writer", func(t *testing.T) {
		t.Parallel()
		client := clientTestAClient(t, func(app *fiber.App) {
			app.Get("/test", func(c fiber.Ctx) error { return c.SendString("test content") })
		})

		resp := clientTestBClientGet(t, client, "http://example.com/test")

		// Create a mock WriteCloser to verify Close is called
		mockWriter := &mockWriteCloser{}
		require.NoError(t, resp.Save(mockWriter))
		require.True(t, mockWriter.closed, "Save() should close io.WriteCloser")
		require.Equal(t, "test content", mockWriter.buf.String())
	})
}

// mockWriteCloser is a helper to verify that Save() closes io.WriteCloser
type mockWriteCloser struct {
	buf    bytes.Buffer
	closed bool
}

type errorReader struct {
	err error
}

func (m *errorReader) Read(_ []byte) (int, error) {
	return 0, m.err
}

type errorWriter struct {
	err error
}

func (m *errorWriter) Write(_ []byte) (int, error) {
	return 0, m.err
}

func (m *mockWriteCloser) Write(p []byte) (int, error) {
	return m.buf.Write(p) //nolint:wrapcheck // propagate buffer write error directly for test helper
}

func (m *mockWriteCloser) Close() error {
	m.closed = true
	return nil
}

// Test_Response_RespondedOrigin covers the fallbacks in the recorded origin.
//
// The pair is what credits a Set-Cookie to the host that actually served the
// response rather than the one the caller addressed. A Response assembled
// outside the normal execution path never has it recorded, and must fall back
// to the request's own URI rather than attributing the cookie to nothing.
func Test_Response_RespondedOrigin(t *testing.T) {
	t.Parallel()

	// Built per subtest: these run in parallel, so a URI released when the
	// parent returns would be reset out from under them.
	askedURI := func(t *testing.T) *fasthttp.URI {
		t.Helper()
		u := fasthttp.AcquireURI()
		t.Cleanup(func() { fasthttp.ReleaseURI(u) })
		require.NoError(t, u.Parse(nil, []byte("http://asked.example/asked")))
		return u
	}

	t.Run("nothing recorded falls back", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBResponse(t)

		host, path := resp.respondedOrigin(askedURI(t))
		require.Equal(t, "asked.example", string(host))
		require.Equal(t, "/asked", string(path))
	})

	t.Run("a nil URI records nothing", func(t *testing.T) {
		t.Parallel()
		resp := clientTestBResponse(t)

		resp.setRespondedURI(nil)

		host, path := resp.respondedOrigin(askedURI(t))
		require.Equal(t, "asked.example", string(host), "a nil URI must leave the fallback in place")
		require.Equal(t, "/asked", string(path))
	})

	t.Run("what was recorded wins", func(t *testing.T) {
		t.Parallel()
		served := fasthttp.AcquireURI()
		t.Cleanup(func() { fasthttp.ReleaseURI(served) })
		require.NoError(t, served.Parse(nil, []byte("http://served.example/served")))

		fallback := askedURI(t)

		resp := clientTestBResponse(t)

		resp.setRespondedURI(served)

		host, path := resp.respondedOrigin(fallback)
		require.Equal(t, "served.example", string(host))
		require.Equal(t, "/served", string(path))

		// Reset clears it, so a pooled Response cannot credit the next
		// response to the previous one's origin.
		resp.Reset()
		host, path = resp.respondedOrigin(fallback)
		require.Equal(t, "asked.example", string(host))
		require.Equal(t, "/asked", string(path))
	})
}

// Test_Response_RespondedOriginOnlyRecordedForAJar covers where the recorded
// origin comes from. Only the cookie-jar hook reads it, so a client without one
// pays nothing to record it, and the fallback answers with the requested URI.
func Test_Response_RespondedOriginOnlyRecordedForAJar(t *testing.T) {
	t.Parallel()
	server := startTestServer(t, func(app *fiber.App) {
		app.Get("/x", func(c fiber.Ctx) error {
			c.Cookie(&fiber.Cookie{Name: "a", Value: "1"})
			return c.SendString("hi")
		})
	})
	t.Cleanup(server.stop)

	t.Run("no jar records nothing", func(t *testing.T) {
		t.Parallel()
		resp, err := New().SetDial(server.dial()).Get("http://example.com/x")
		require.NoError(t, err)
		defer resp.Close()
		require.Empty(t, resp.respondedHost)
		require.Empty(t, resp.respondedPath)
	})

	t.Run("a jar gets the origin", func(t *testing.T) {
		t.Parallel()
		jar := AcquireCookieJar()
		defer jar.Release()
		resp, err := New().SetDial(server.dial()).SetCookieJar(jar).Get("http://example.com/x")
		require.NoError(t, err)
		defer resp.Close()
		require.Equal(t, "example.com", string(resp.respondedHost))
		require.Equal(t, "/x", string(resp.respondedPath))
	})
}
