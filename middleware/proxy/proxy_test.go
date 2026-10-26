package proxy

import (
	"bufio"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	clientpkg "github.com/gofiber/fiber/v3/client"
	"github.com/gofiber/utils/v2"
	"github.com/stretchr/testify/require"

	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/gofiber/fiber/v3/internal/tlstest"
	"github.com/valyala/fasthttp"
)

// TestMain relaxes the proxy security policy for the whole suite so the
// existing loopback-based tests continue to work. Tests that exercise
// the secure defaults install their own policy in their own scope.
func TestMain(m *testing.M) {
	WithSecurityPolicy(proxyTestAllowPrivatePolicy())
	m.Run()
}

func startServer(app *fiber.App, ln net.Listener) {
	go func() {
		err := app.Listener(ln, fiber.ListenConfig{
			DisableStartupMessage: true,
		})
		if err != nil {
			panic(err)
		}
	}()
}

func createProxyTestServer(t *testing.T, handler fiber.Handler, network, address string) (target *fiber.App, addr string) { //nolint:nonamedreturns // gocritic unnamedResult prefers naming returned target app and address for readability
	t.Helper()

	target = fiber.New()
	target.Get("/", handler)

	ln, err := net.Listen(network, address)
	require.NoError(t, err)

	addr = ln.Addr().String()

	startServer(target, ln)

	return target, addr
}

func createProxyTestServerIPv4(t *testing.T, handler fiber.Handler) (target *fiber.App, addr string) { //nolint:nonamedreturns // gocritic unnamedResult prefers naming returned target app and address for readability
	t.Helper()
	return createProxyTestServer(t, handler, fiber.NetworkTCP4, "127.0.0.1:0")
}

func createProxyTestServerIPv6(t *testing.T, handler fiber.Handler) (target *fiber.App, addr string) { //nolint:nonamedreturns // gocritic unnamedResult prefers naming returned target app and address for readability
	t.Helper()

	// Skip instead of failing on hosts without IPv6 support (e.g. some CI containers).
	probe, err := net.Listen(fiber.NetworkTCP6, "[::1]:0")
	if err != nil {
		t.Skipf("skipping: IPv6 is not available: %v", err)
	}
	require.NoError(t, probe.Close())

	return createProxyTestServer(t, handler, fiber.NetworkTCP6, "[::1]:0")
}

func createRedirectServer(t *testing.T) string {
	t.Helper()
	app := fiber.New()

	var addr string
	app.Get("/", func(c fiber.Ctx) error {
		c.Location("http://" + addr + "/final")
		return c.Status(fiber.StatusMovedPermanently).SendString("redirect")
	})
	app.Get("/final", func(c fiber.Ctx) error {
		return c.SendString("final")
	})

	ln, err := net.Listen(fiber.NetworkTCP4, "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		ln.Close() //nolint:errcheck // It is fine to ignore the error here
	})
	addr = ln.Addr().String()

	startServer(app, ln)

	return addr
}

func restoreGlobalProxyClient(tb testing.TB) {
	tb.Helper()

	prev := sharedClient.Load()
	tb.Cleanup(func() {
		WithClient(prev)
	})
}

// proxyTestAllowPrivatePolicy returns the default security policy with private
// upstream addresses allowed, which the loopback servers of the tests need.
func proxyTestAllowPrivatePolicy() SecurityPolicy {
	policy := DefaultSecurityPolicy()
	policy.AllowPrivateIPs = true
	return policy
}

// proxyTestTLSListener returns a loopback TCP listener that speaks TLS with the
// test certificate, to serve an app behind.
func proxyTestTLSListener(t *testing.T) net.Listener {
	t.Helper()

	tlsConf, _, err := tlstest.GetTLSConfigs()
	require.NoError(t, err)
	ln, err := net.Listen(fiber.NetworkTCP4, "127.0.0.1:0")
	require.NoError(t, err)
	return tls.NewListener(ln, tlsConf)
}

// proxyTestTLSClient returns a client that reaches a server presenting the test
// certificate, which it does not verify.
func proxyTestTLSClient() *clientpkg.Client {
	client := clientpkg.New()
	client.SetTimeout(5 * time.Second)
	client.TLSConfig().InsecureSkipVerify = true
	return client
}

// proxyTestExpectGet gets target with client and requires the status 200 and
// the given body.
func proxyTestExpectGet(t *testing.T, client *clientpkg.Client, target, body string) {
	t.Helper()

	resp, err := client.Get(target)
	require.NoError(t, err)
	defer resp.Close()
	require.Equal(t, fiber.StatusOK, resp.StatusCode(), string(resp.Body()))
	require.Equal(t, body, string(resp.Body()))
}

// proxyTestFieldLinesUpstream starts an upstream that answers body and hands
// the field lines of the request that match names (see matchingFieldLines) to
// the returned channel, which holds the one request a test sends it.
func proxyTestFieldLinesUpstream(t *testing.T, body string, names ...string) (addr string, seen <-chan []string) { //nolint:nonamedreturns // gocritic unnamedResult prefers naming the returned address and channel for readability
	t.Helper()

	lines := make(chan []string, 1)
	_, addr = createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		lines <- matchingFieldLines(c, names...)
		return c.SendString(body)
	})
	return addr, lines
}

// go test -run Test_Proxy_DefaultClient_MaxConnsPerHost
func Test_Proxy_DefaultClient_MaxConnsPerHost(t *testing.T) {
	t.Parallel()
	require.Equal(t, defaultMaxConnsPerHost, sharedClient.Load().MaxConnsPerHost)
}

// Test_Proxy_Balancer_LeavesConfiguredClientAlone pins that Balancer does not write to a host client
// that already has DisablePathNormalizing: another goroutine may be reading it (run under -race).
func Test_Proxy_Balancer_LeavesConfiguredClientAlone(t *testing.T) {
	t.Parallel()

	hc := &fasthttp.HostClient{Addr: "127.0.0.1:1", DisablePathNormalizing: true}
	var reads atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			if hc.DisablePathNormalizing {
				reads.Add(1)
			}
		}
	})
	t.Cleanup(func() {
		stop.Store(true)
		wg.Wait()
	})
	readsMoreThan := func(n int64) func() bool { return func() bool { return reads.Load() > n } }

	require.Eventually(t, readsMoreThan(0), 5*time.Second, time.Millisecond, "the reader is running before the build")

	Balancer(Config{Client: &fasthttp.LBClient{Clients: []fasthttp.BalancingClient{hc}}})

	require.Eventually(t, readsMoreThan(reads.Load()+1000), 5*time.Second, time.Millisecond, "and is still reading after it")
}

// go test -run Test_Proxy_ConfigDefault_MaxConnsPerHost
func Test_Proxy_ConfigDefault_MaxConnsPerHost(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		set  int // Config.MaxConnsPerHost, 0 to leave it unset
		want int
	}{
		{name: "default", want: defaultMaxConnsPerHost},
		{name: "override", set: 2048, want: 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := configDefault(Config{
				Servers:         []string{"127.0.0.1"},
				MaxConnsPerHost: tc.set,
			})
			require.Equal(t, tc.want, cfg.MaxConnsPerHost)
		})
	}
}

// go test -run Test_Proxy_EmptyServersPanic
//
// Without a server to forward to there is nothing a request could be sent to, so
// the constructors stop at startup.
func Test_Proxy_EmptyServersPanic(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		build func()
		name  string
	}{
		{name: "Balancer with empty Servers", build: func() { Balancer(Config{Servers: []string{}}) }},
		{name: "Balancer with empty Config", build: func() { Balancer(Config{}) }},
		// No default upstream exists, so Balancer() stops at startup, as Balancer(Config{}) does.
		{name: "Balancer without arguments", build: func() { Balancer() }},
		{name: "BalancerForward with empty servers", build: func() { BalancerForward([]string{}) }},
		// Neither Servers nor a Client override is supplied.
		{name: "configDefault with neither Servers nor Client", build: func() { configDefault(Config{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.PanicsWithValue(t, "Servers cannot be empty", tc.build)
		})
	}
}

// go test -run Test_Proxy_Next
func Test_Proxy_Next(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(Balancer(Config{
		Servers: []string{"127.0.0.1"},
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	apptest.Expect(t, app, "/", fiber.StatusNotFound)
}

// go test -run Test_Proxy_Balancer_Upstream
//
// Each row starts an upstream that answers 418, checks that it does, and has a
// Balancer reach it from an app of its own.
func Test_Proxy_Balancer_Upstream(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		server   func(*testing.T, fiber.Handler) (*fiber.App, string)
		balancer func(addr string) Config
		name     string
		appCfg   []fiber.Config
		status   int
	}{
		{
			name:     "IPv4",
			server:   createProxyTestServerIPv4,
			balancer: func(addr string) Config { return Config{Servers: []string{addr}} },
			status:   fiber.StatusTeapot,
		},
		{
			name:     "IPv6",
			server:   createProxyTestServerIPv6,
			balancer: func(addr string) Config { return Config{Servers: []string{addr}} },
			status:   fiber.StatusInternalServerError,
		},
		{
			name:   "IPv6 with DialDualStack",
			server: createProxyTestServerIPv6,
			balancer: func(addr string) Config {
				return Config{Servers: []string{addr}, DialDualStack: true}
			},
			status: fiber.StatusTeapot,
		},
		{
			name:   "IPv4 with DialDualStack",
			server: createProxyTestServerIPv4,
			balancer: func(addr string) Config {
				return Config{Servers: []string{addr}, DialDualStack: true}
			},
			status: fiber.StatusTeapot,
		},
		{
			name:   "custom LBClient",
			server: createProxyTestServerIPv4,
			balancer: func(addr string) Config {
				return Config{Client: &fasthttp.LBClient{
					Clients: []fasthttp.BalancingClient{
						&fasthttp.HostClient{
							NoDefaultUserAgentHeader: true,
							DisablePathNormalizing:   true,
							Addr:                     addr,
						},
					},
					Timeout: time.Second,
				}}
			},
			status: fiber.StatusTeapot,
		},
		{
			name:     "immutable app",
			server:   createProxyTestServerIPv4,
			balancer: func(addr string) Config { return Config{Servers: []string{addr}} },
			appCfg:   []fiber.Config{{Immutable: true}},
			status:   fiber.StatusTeapot,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target, addr := tc.server(t, func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusTeapot)
			})

			resp := apptest.Do(t, target, "/", fiber.TestConfig{
				Timeout:       2 * time.Second,
				FailOnTimeout: true,
			})
			require.Equal(t, fiber.StatusTeapot, resp.StatusCode)

			app := fiber.New(tc.appCfg...)
			app.Use(Balancer(tc.balancer(addr)))

			apptest.Expect(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHost(addr)), tc.status)
		})
	}
}

// go test -run Test_Proxy_Balancer_WithTlsConfig
//
// The balancer does not speak TLS itself, but the request must still reach the upstream as https.
func Test_Proxy_Balancer_WithTlsConfig(t *testing.T) {
	t.Parallel()

	ln := proxyTestTLSListener(t)

	upstream := fiber.New()
	upstream.Get("/tlsbalancer", func(c fiber.Ctx) error {
		return c.SendString("tls balancer")
	})
	startServer(upstream, ln)

	app := fiber.New()
	// disable certificate verification in Balancer
	app.Use(Balancer(Config{
		Servers:   []string{"https://" + ln.Addr().String()},
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}))

	resp := apptest.Do(t, app, "/tlsbalancer", fiber.TestConfig{
		Timeout:       5 * time.Second,
		FailOnTimeout: true,
	})
	body := apptest.Body(t, resp)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, fiber.StatusOK, resp.StatusCode, body)
	require.Equal(t, "tls balancer", body)
}

// go test -run Test_Proxy_Balancer_PlainUpstreamBehindTLS
//
// The other way round: the balancer sits behind a TLS listener and its upstream is plain HTTP.
func Test_Proxy_Balancer_PlainUpstreamBehindTLS(t *testing.T) {
	t.Parallel()

	_, upstreamAddr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString("plain upstream")
	})

	ln := proxyTestTLSListener(t)
	gatewayAddr := ln.Addr().String()

	app := fiber.New()
	app.Use(Balancer(Config{Servers: []string{"http://" + upstreamAddr}}))
	startServer(app, ln)

	proxyTestExpectGet(t, proxyTestTLSClient(), "https://"+gatewayAddr, "plain upstream")
}

// go test -run Test_Proxy_Balancer_MixedSchemes
//
// Each upstream of a mixed list is reached with its own scheme, whichever one the request arrived on.
func Test_Proxy_Balancer_MixedSchemes(t *testing.T) {
	t.Parallel()

	_, plainAddr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString("plain")
	})
	ln := proxyTestTLSListener(t)
	secure := fiber.New()
	secure.Get("/", func(c fiber.Ctx) error { return c.SendString("secure") })
	startServer(secure, ln)

	app := fiber.New()
	app.Use(Balancer(Config{
		Servers:   []string{"http://" + plainAddr, "https://" + ln.Addr().String()},
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}))

	seen := map[string]int{}
	for range 6 {
		resp := apptest.Do(t, app, "/", fiber.TestConfig{
			Timeout:       5 * time.Second,
			FailOnTimeout: true,
		})
		body := apptest.Body(t, resp)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, fiber.StatusOK, resp.StatusCode, body)
		seen[body]++
	}
	require.Equal(t, map[string]int{"plain": 3, "secure": 3}, seen)
}

// go test -run Test_Proxy_Balancer_EmptyClientPanicsAtStartup
func Test_Proxy_Balancer_EmptyClientPanicsAtStartup(t *testing.T) {
	t.Parallel()

	// fasthttp would panic on the first request, and nothing above the handler recovers it.
	require.PanicsWithValue(t, "Client.Clients cannot be empty", func() {
		Balancer(Config{Client: &fasthttp.LBClient{}})
	})
}

// go test -run Test_Proxy_Forward_WithTlsConfig_To_Http
func Test_Proxy_Forward_WithTlsConfig_To_Http(t *testing.T) {
	t.Parallel()

	_, targetAddr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString("hello from target")
	})

	proxyServerLn := proxyTestTLSListener(t)
	proxyAddr := proxyServerLn.Addr().String()

	app := fiber.New()
	app.Use(Forward("http://" + targetAddr))
	startServer(app, proxyServerLn)

	proxyTestExpectGet(t, proxyTestTLSClient(), "https://"+proxyAddr, "hello from target")
}

// Test_Proxy_DoRedirects_AppliesSchemePerHopBehindTLS pins that every hop is sent with the scheme of
// its own URL: a copied hop would keep the TLS mark of the gateway's request and speak TLS to plain HTTP.
func Test_Proxy_DoRedirects_AppliesSchemePerHopBehindTLS(t *testing.T) {
	t.Parallel()

	_, finalAddr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString("final")
	})
	_, redirectAddr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		c.Location("http://" + finalAddr + "/")
		return c.Status(fiber.StatusFound).SendString("redirect")
	})

	ln := proxyTestTLSListener(t)
	gatewayAddr := ln.Addr().String()

	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		return DoRedirects(c, "http://"+redirectAddr+"/", 3)
	})
	startServer(app, ln)

	proxyTestExpectGet(t, proxyTestTLSClient(), "https://"+gatewayAddr, "final")
}

// Test_Proxy_DoRedirects_StreamedRequestBody pins that a method-keeping redirect of a streamed
// request (StreamRequestBody) is followed when the first hop left the whole body in memory.
func Test_Proxy_DoRedirects_StreamedRequestBody(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		body       string
		wantBody   string
		wantStatus int
		wantFinals int32
	}{
		{name: "empty", wantStatus: fiber.StatusOK, wantBody: "POST:0", wantFinals: 1},
		{name: "small", body: "payload", wantStatus: fiber.StatusOK, wantBody: "POST:7", wantFinals: 1},
		{name: "more than is read ahead", body: strings.Repeat("a", 8*1024+1), wantStatus: fiber.StatusInternalServerError, wantBody: fasthttp.ErrRedirectBodyStream.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var finals atomic.Int32 // requests that reached the target of the redirect
			upstream := fiber.New()
			ln, err := net.Listen(fiber.NetworkTCP4, "127.0.0.1:0")
			require.NoError(t, err)
			addr := ln.Addr().String()
			upstream.Post("/start", func(c fiber.Ctx) error {
				return c.Redirect().Status(fiber.StatusTemporaryRedirect).To("http://" + addr + "/final")
			})
			upstream.Post("/final", func(c fiber.Ctx) error {
				finals.Add(1)
				return c.SendString(c.Method() + ":" + strconv.Itoa(len(c.Body())))
			})
			startServer(upstream, ln)

			app := fiber.New(fiber.Config{StreamRequestBody: true})
			app.Post("/", func(c fiber.Ctx) error {
				return DoRedirects(c, "http://"+addr+"/start", 3)
			})

			req := httptest.NewRequest(fiber.MethodPost, "/", strings.NewReader(tc.body))
			req.Header.Set(fiber.HeaderContentLength, strconv.Itoa(len(tc.body)))
			resp := apptest.Do(t, app, req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
			defer func() { require.NoError(t, resp.Body.Close()) }()

			body := apptest.Body(t, resp)
			require.Equal(t, tc.wantStatus, resp.StatusCode)
			require.Equal(t, tc.wantBody, body)
			require.Equal(t, tc.wantFinals, finals.Load(), "requests that reached the target of the redirect")
		})
	}
}

// go test -run Test_Proxy_Forward
func Test_Proxy_Forward(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString("forwarded")
	})

	app.Use(Forward("http://" + addr))

	apptest.ExpectBody(t, app, "/", fiber.StatusOK, "forwarded")
}

// Test_Proxy_RealIPOverwritesClientClaims checks that every helper that writes
// X-Real-IP leaves the upstream exactly one line, the address Fiber saw, however
// many lines the client sent.
//
// The forwarders overwrite X-Real-IP so the upstream can attribute the request to
// the peer Fiber actually saw. Header.Set replaces the first field line with a
// given name and leaves any others in place, so a client that sends the header
// twice used to keep one of its own values on the wire — and the upstream, which
// may read the last line or join the pair per RFC 9110 Section 5.2, would
// attribute the request to an address the client chose.
func Test_Proxy_RealIPOverwritesClientClaims(t *testing.T) {
	t.Parallel()

	for _, handler := range realIPHandlers() {
		for _, claim := range []struct {
			name   string
			values []string
		}{
			{name: "one spoofed line", values: []string{"10.0.0.1"}},
			{name: "two spoofed lines", values: []string{"7.7.7.7", "8.8.8.8"}},
		} {
			t.Run(handler.name+" "+claim.name, func(t *testing.T) {
				t.Parallel()

				addr, seen := proxyTestFieldLinesUpstream(t, "OK", realIPHeader)

				app := fiber.New()
				app.Use(handler.build(addr))

				req := apptest.Req(fiber.MethodGet, "/", apptest.WithHost("example.com"))
				for _, value := range claim.values {
					req.Header.Add("X-Real-IP", value)
				}

				apptest.Expect(t, app, req, fiber.StatusOK)
				// app.Test injects 0.0.0.0 as the remote address.
				require.Equal(t, []string{"X-Real-Ip=0.0.0.0"}, <-seen)
			})
		}
	}
}

// Test_Proxy_Forward_RealIPFromProxyHeader pins that replacing X-Real-IP does
// not destroy the value it is derived from.
//
// With Config.ProxyHeader set to "X-Real-IP", c.IP() reads that very header, so
// deleting it before resolving the address left the upstream with an empty
// X-Real-IP instead of the client's.
func Test_Proxy_Forward_RealIPFromProxyHeader(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		seen := c.Request().Header.PeekAll("X-Real-IP")
		out := make([]string, 0, len(seen))
		for _, v := range seen {
			out = append(out, string(v))
		}
		return c.SendString(strings.Join(out, "|"))
	})

	app := fiber.New(fiber.Config{
		TrustProxy:       true,
		TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"0.0.0.0/0"}},
		ProxyHeader:      "X-Real-IP",
	})
	app.Use(Forward("http://" + addr))

	req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
	req.Header.Set("X-Real-IP", "203.0.113.9")

	apptest.ExpectBody(t, app, req, fiber.StatusOK, "203.0.113.9")
}

// realIPHandler names one of the helpers that write X-Real-IP, with a
// constructor taking the upstream address.
type realIPHandler struct {
	build func(addr string) fiber.Handler
	name  string
}

// realIPHandlers returns every helper that writes X-Real-IP. DomainForward is
// built for example.com, so requests carry that Host.
func realIPHandlers() []realIPHandler {
	return []realIPHandler{
		{name: "Forward", build: func(addr string) fiber.Handler { return Forward("http://" + addr) }},
		{name: "DomainForward", build: func(addr string) fiber.Handler { return DomainForward("example.com", "http://"+addr) }},
		{name: "BalancerForward", build: func(addr string) fiber.Handler { return BalancerForward([]string{"http://" + addr}) }},
		{name: "Balancer", build: func(addr string) fiber.Handler { return Balancer(Config{Servers: []string{addr}}) }},
	}
}

// Test_Proxy_ConnectionCannotRemoveRealIP checks that a client naming X-Real-IP
// in Connection does not have the line this hop writes removed: the listing is
// applied to the request as received, and X-Real-IP is this hop's to write.
func Test_Proxy_ConnectionCannotRemoveRealIP(t *testing.T) {
	t.Parallel()

	for _, tc := range realIPHandlers() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			addr, seen := proxyTestFieldLinesUpstream(t, "OK", realIPHeader)

			app := fiber.New()
			app.Use(tc.build(addr))

			req := apptest.Req(fiber.MethodGet, "/", apptest.WithHost("example.com"),
				apptest.WithHeader(fiber.HeaderConnection, "X-Real-IP"),
				apptest.WithHeader(realIPHeader, "7.7.7.7"))

			apptest.Expect(t, app, req, fiber.StatusOK)
			// app.Test injects 0.0.0.0 as the remote address.
			require.Equal(t, []string{"X-Real-Ip=0.0.0.0"}, <-seen)
		})
	}
}

// Test_Proxy_ConnectionCannotRemoveRealIPWhateverTheCase is the same under
// DisableHeaderNormalizing, where the listing and the field arrive spelled as
// the client sent them.
func Test_Proxy_ConnectionCannotRemoveRealIPWhateverTheCase(t *testing.T) {
	t.Parallel()

	for _, tc := range realIPHandlers() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			addr, seen := proxyTestFieldLinesUpstream(t, "upstream", realIPHeader)

			app := fiber.New(fiber.Config{DisableHeaderNormalizing: true})
			app.Use(tc.build(addr))

			body := sendRawUnnormalized(t, app, "GET / HTTP/1.1\r\nHost: example.com\r\n"+
				"connection: x-real-ip\r\n"+
				"x-real-ip: 7.7.7.7\r\n\r\n")
			require.Equal(t, "upstream", body)

			lines := <-seen
			require.Len(t, lines, 1, "exactly one X-Real-IP reaches the upstream: %v", lines)
			require.NotContains(t, lines[0], "7.7.7.7")
		})
	}
}

// Test_Proxy_ConnectionCannotRemoveForwardingHeaders checks that the fields an
// application writes about the client before the proxy runs are not the
// client's to remove by naming them in Connection, while any other field it
// names there still is, as RFC 9110 §7.6.1 asks.
func Test_Proxy_ConnectionCannotRemoveForwardingHeaders(t *testing.T) {
	t.Parallel()

	const (
		clientIP  = "203.0.113.9"
		forwarded = "for=203.0.113.9;proto=https"
	)

	for _, tc := range realIPHandlers() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			addr, seen := proxyTestFieldLinesUpstream(t, "OK", fiber.HeaderXForwardedFor, fiber.HeaderXForwardedHost,
				fiber.HeaderXForwardedProto, fiber.HeaderForwarded, "X-Gateway-Policy")

			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				h := &c.Request().Header
				h.Set(fiber.HeaderXForwardedFor, clientIP)
				h.Set(fiber.HeaderXForwardedHost, "app.example")
				h.Set(fiber.HeaderXForwardedProto, "https")
				h.Set(fiber.HeaderForwarded, forwarded)
				h.Set("X-Gateway-Policy", "read-only")
				return c.Next()
			})
			app.Use(tc.build(addr))

			req := apptest.Req(fiber.MethodGet, "/", apptest.WithHost("example.com"),
				apptest.WithHeader(fiber.HeaderConnection,
					"X-Forwarded-For, X-Forwarded-Host, X-Forwarded-Proto, Forwarded, X-Gateway-Policy"))

			apptest.Expect(t, app, req, fiber.StatusOK)
			require.ElementsMatch(t, []string{
				"X-Forwarded-For=" + clientIP,
				"X-Forwarded-Host=app.example",
				"X-Forwarded-Proto=https",
				"Forwarded=" + forwarded,
			}, <-seen, "the forwarding fields reach the upstream; X-Gateway-Policy, which the listing may remove, does not")
		})
	}
}

// Test_Proxy_RealIPOutlivesTheTrailerReturn checks that the address written
// into X-Real-IP is the one the request carried when the field it was read
// from is deleted before the write. Removing Trailer returns the received
// trailer fields to the header section, where fasthttp reuses the buffers of
// the lines deleted before them, so an address still aliasing such a buffer
// read as whatever trailer landed on it.
func Test_Proxy_RealIPOutlivesTheTrailerReturn(t *testing.T) {
	t.Parallel()

	for _, tc := range realIPHandlers() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			addr, seen := proxyTestFieldLinesUpstream(t, "upstream", realIPHeader)

			app := fiber.New(fiber.Config{
				TrustProxy:       true,
				TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"0.0.0.0/0"}},
				ProxyHeader:      "CF-Connecting-IP",
			})
			app.Use(tc.build(addr))

			// The peer names the proxy header in Connection, so that field is
			// deleted before Trailer is, and sends enough trailer fields for
			// one of them to land on its buffer. A GET, since that is the
			// route the upstream serves; a body on it is framed all the same.
			body := sendRaw(t, app, "GET / HTTP/1.1\r\nHost: example.com\r\n"+
				"CF-Connecting-IP: 203.0.113.5\r\n"+
				"Connection: CF-Connecting-IP\r\n"+
				"Transfer-Encoding: chunked\r\n\r\n"+
				"5\r\nhello\r\n0\r\n"+
				"X-A: 192.168.1.1\r\nX-B: 192.168.1.1\r\nX-C: 192.168.1.1\r\n\r\n")
			require.Equal(t, "upstream", body)
			require.Equal(t, []string{"X-Real-Ip=203.0.113.5"}, <-seen)
		})
	}
}

// Test_Proxy_Do_ConnectionCannotRemoveRealIP checks that the X-Real-IP written
// by hand before Do, as the docs show, is not the client's to remove either.
func Test_Proxy_Do_ConnectionCannotRemoveRealIP(t *testing.T) {
	t.Parallel()

	addr, seen := proxyTestFieldLinesUpstream(t, "OK", realIPHeader)

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		ip := c.IP()
		c.Request().Header.Del(realIPHeader)
		c.Request().Header.Add(realIPHeader, ip)
		return Do(c, "http://"+addr)
	})

	req := apptest.Req(fiber.MethodGet, "/",
		apptest.WithHeader(fiber.HeaderConnection, "X-Real-IP"),
		apptest.WithHeader(realIPHeader, "7.7.7.7"))

	apptest.Expect(t, app, req, fiber.StatusOK)
	require.Equal(t, []string{"X-Real-Ip=0.0.0.0"}, <-seen)
}

// go test -run Test_Proxy_Forward_WithClient_TLSConfig
func Test_Proxy_Forward_WithClient_TLSConfig(t *testing.T) {
	// Intentionally not parallel: WithClient replaces the process-wide shared client.
	restoreGlobalProxyClient(t)

	ln := proxyTestTLSListener(t)

	app := fiber.New()

	app.Get("/tlsfwd", func(c fiber.Ctx) error {
		return c.SendString("tls forward")
	})

	addr := ln.Addr().String()
	clientTLSConf := &tls.Config{InsecureSkipVerify: true}

	// disable certificate verification
	WithClient(&fasthttp.Client{
		TLSConfig: clientTLSConf,
	})
	app.Use(Forward("https://" + addr + "/tlsfwd"))

	startServer(app, ln)

	client := clientpkg.New()
	client.SetTLSConfig(clientTLSConf)

	proxyTestExpectGet(t, client, "https://"+addr, "tls forward")
}

// go test -run Test_Proxy_Modify_Response
func Test_Proxy_Modify_Response(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.Status(500).SendString("not modified")
	})

	app := fiber.New()
	app.Use(Balancer(Config{
		Servers: []string{addr},
		ModifyResponse: func(c fiber.Ctx) error {
			c.Response().SetStatusCode(fiber.StatusOK)
			return c.SendString("modified response")
		},
	}))

	apptest.ExpectBody(t, app, "/", fiber.StatusOK, "modified response")
}

// go test -run Test_Proxy_Modify_Request
func Test_Proxy_Modify_Request(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		b := c.Request().Body()
		return c.SendString(string(b))
	})

	app := fiber.New()
	app.Use(Balancer(Config{
		Servers: []string{addr},
		ModifyRequest: func(c fiber.Ctx) error {
			c.Request().SetBody([]byte("modified request"))
			return nil
		},
	}))

	apptest.ExpectBody(t, app, "/", fiber.StatusOK, "modified request")
}

// go test -run Test_Proxy_Timeout
func Test_Proxy_Timeout(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		body    string
		delay   time.Duration // how long the upstream takes to answer
		timeout time.Duration // Config.Timeout
		status  int
	}{
		{name: "slow server within the timeout", delay: 300 * time.Millisecond, timeout: 600 * time.Millisecond, status: fiber.StatusOK, body: "fiber is awesome"},
		{name: "server slower than the timeout", delay: time.Second, timeout: 100 * time.Millisecond, status: fiber.StatusInternalServerError, body: fasthttp.ErrTimeout.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
				time.Sleep(tc.delay)
				return c.SendString("fiber is awesome")
			})

			app := fiber.New()
			app.Use(Balancer(Config{
				Servers: []string{addr},
				Timeout: tc.timeout,
			}))

			resp := apptest.Do(t, app, "/", fiber.TestConfig{
				Timeout:       2 * time.Second,
				FailOnTimeout: true,
			})
			require.Equal(t, tc.status, resp.StatusCode)

			require.Equal(t, tc.body, apptest.Body(t, resp))
		})
	}
}

// go test -run Test_Proxy_Buffer_Size_Response
func Test_Proxy_Buffer_Size_Response(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		long := strings.Join(make([]string, 5000), "-")
		c.Set("Very-Long-Header", long)
		return c.SendString("ok")
	})

	app := fiber.New()
	app.Use(Balancer(Config{Servers: []string{addr}, KeepConnectionHeader: true}))

	apptest.Expect(t, app, "/", fiber.StatusInternalServerError)

	app = fiber.New()
	app.Use(Balancer(Config{
		Servers:        []string{addr},
		ReadBufferSize: 1024 * 8,
	}))

	apptest.Expect(t, app, "/", fiber.StatusOK)
}

// go test -race -run Test_Proxy_DoHelpers
//
// Every Do helper answers with what its upstream sent, or the error it ended in,
// and leaves the request URL of the app's own request as it was.
func Test_Proxy_DoHelpers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		do       func(c fiber.Ctx, target string) error
		name     string
		body     string // what the helper has to answer with, which the upstream says too unless it redirects
		status   int
		redirect bool // whether the upstream redirects to a page of its own instead of answering
	}{
		{
			name:   "Do",
			do:     func(c fiber.Ctx, target string) error { return Do(c, target) },
			status: fiber.StatusOK,
			body:   "proxied",
		},
		{
			name:   "Do with the real URL",
			do:     func(c fiber.Ctx, target string) error { return Do(c, target) },
			status: fiber.StatusOK,
			body:   "real url",
		},
		{
			name:     "Do does not follow a redirect",
			do:       func(c fiber.Ctx, target string) error { return Do(c, target) },
			redirect: true,
			status:   fiber.StatusMovedPermanently,
			body:     "redirect",
		},
		{
			name:     "DoRedirects",
			do:       func(c fiber.Ctx, target string) error { return DoRedirects(c, target, 1) },
			redirect: true,
			status:   fiber.StatusOK,
			body:     "final",
		},
		{
			name:     "DoRedirects with too many redirects",
			do:       func(c fiber.Ctx, target string) error { return DoRedirects(c, target, 0) },
			redirect: true,
			status:   fiber.StatusInternalServerError,
			body:     fasthttp.ErrTooManyRedirects.Error(),
		},
		{
			name:   "DoTimeout",
			do:     func(c fiber.Ctx, target string) error { return DoTimeout(c, target, time.Second) },
			status: fiber.StatusOK,
			body:   "proxied",
		},
		{
			name: "DoDeadline",
			do: func(c fiber.Ctx, target string) error {
				return DoDeadline(c, target, time.Now().Add(time.Second))
			},
			status: fiber.StatusOK,
			body:   "proxied",
		},
		{
			name: "Do with a client of its own",
			do: func(c fiber.Ctx, target string) error {
				return Do(c, target, &fasthttp.Client{
					NoDefaultUserAgentHeader: true,
					DisablePathNormalizing:   true,
				})
			},
			status: fiber.StatusOK,
			body:   "proxied",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var addr string
			if tc.redirect {
				addr = createRedirectServer(t)
			} else {
				_, addr = createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
					return c.SendString(tc.body)
				})
			}

			app := fiber.New()
			app.Get("/test", func(c fiber.Ctx) error {
				return tc.do(c, "http://"+addr)
			})

			resp := apptest.Do(t, app, "/test", fiber.TestConfig{
				Timeout:       2 * time.Second,
				FailOnTimeout: true,
			})
			require.Equal(t, tc.body, apptest.Body(t, resp))
			require.Equal(t, tc.status, resp.StatusCode)
			require.Equal(t, "/test", resp.Request.URL.String())
		})
	}
}

// go test -race -run Test_Proxy_DoRedirects_AliasedAddr
// Test_Proxy_DoRedirects_AliasedAddr covers an addr that aliases the request buffer (c.OriginalURL()):
// doActionWithPolicy clones it once, or SetRequestURI would corrupt a *url.URL still pointing into it.
func Test_Proxy_DoRedirects_AliasedAddr(t *testing.T) {
	t.Parallel()

	addr := createRedirectServer(t)
	app := fiber.New()
	// Routing the upstream through the request path forces addr to alias
	// req.Header.requestURI, exercising the aliasing case directly.
	app.Get("/*", func(c fiber.Ctx) error {
		aliased := strings.TrimPrefix(c.OriginalURL(), "/")
		return DoRedirects(c, aliased, 1)
	})

	resp := apptest.Do(t, app, "/http://"+addr, fiber.TestConfig{
		Timeout:       2 * time.Second,
		FailOnTimeout: true,
	})
	require.Equal(t, "final", apptest.Body(t, resp))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

// go test -race -run Test_Proxy_DoTimeout_Timeout
func Test_Proxy_DoTimeout_Timeout(t *testing.T) {
	// Intentionally not parallel: it measures a timeout, so it must not compete for CPU.
	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		time.Sleep(time.Second * 5)
		return c.SendString("proxied")
	})

	app := fiber.New()
	app.Get("/test", func(c fiber.Ctx) error {
		return DoTimeout(c, "http://"+addr, time.Second)
	})

	resp := apptest.Do(t, app, "/test", fiber.TestConfig{
		Timeout:       2 * time.Second,
		FailOnTimeout: true,
	})
	require.Equal(t, fasthttp.ErrTimeout.Error(), apptest.Body(t, resp))
	require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, "/test", resp.Request.URL.String())
}

// go test -race -run Test_Proxy_DoDeadline_PastDeadline
func Test_Proxy_DoDeadline_PastDeadline(t *testing.T) {
	// Intentionally not parallel: it measures a deadline, so it must not compete for CPU.
	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		time.Sleep(time.Second * 5)
		return c.SendString("proxied")
	})

	app := fiber.New()
	app.Get("/test", func(c fiber.Ctx) error {
		return DoDeadline(c, "http://"+addr, time.Now().Add(2*time.Second))
	})

	_, err1 := app.Test(httptest.NewRequest(fiber.MethodGet, "/test", http.NoBody), fiber.TestConfig{
		Timeout:       1 * time.Second,
		FailOnTimeout: true,
	})
	require.Equal(t, os.ErrDeadlineExceeded, err1)
}

// go test -race -run Test_Proxy_Do_HTTP_Prefix_URL
func Test_Proxy_Do_HTTP_Prefix_URL(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString("hello world")
	})

	app := fiber.New()
	app.Get("/*", func(c fiber.Ctx) error {
		path := c.OriginalURL()
		url := strings.TrimPrefix(path, "/")

		require.Equal(t, "http://"+addr, url)
		if err := Do(c, url); err != nil {
			return err
		}
		c.Response().Header.Del(fiber.HeaderServer)
		return nil
	})

	resp := apptest.Do(t, app, "/http://"+addr)
	require.Equal(t, "hello world", apptest.Body(t, resp))
}

// go test -race -run Test_Proxy_Forward_Global_Client
func Test_Proxy_Forward_Global_Client(t *testing.T) {
	// Intentionally not parallel: WithClient replaces the process-wide shared client.
	restoreGlobalProxyClient(t)
	ln, err := net.Listen(fiber.NetworkTCP4, "127.0.0.1:0")
	require.NoError(t, err)
	WithClient(&fasthttp.Client{
		NoDefaultUserAgentHeader: true,
		DisablePathNormalizing:   true,
		MaxConnsPerHost:          123,
	})
	loadedClient := sharedClient.Load()
	require.NotNil(t, loadedClient)
	require.Equal(t, 123, loadedClient.MaxConnsPerHost)

	app := fiber.New()
	app.Get("/test_global_client", func(c fiber.Ctx) error {
		return c.SendString("test_global_client")
	})

	addr := ln.Addr().String()
	app.Use(Forward("http://" + addr + "/test_global_client"))
	startServer(app, ln)

	proxyTestExpectGet(t, clientpkg.New(), "http://"+addr, "test_global_client")
}

// go test -race -run Test_Proxy_Forward_Local_Client
func Test_Proxy_Forward_Local_Client(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen(fiber.NetworkTCP4, "127.0.0.1:0")
	require.NoError(t, err)
	app := fiber.New()
	app.Get("/test_local_client", func(c fiber.Ctx) error {
		return c.SendString("test_local_client")
	})

	addr := ln.Addr().String()
	app.Use(Forward("http://"+addr+"/test_local_client", &fasthttp.Client{
		NoDefaultUserAgentHeader: true,
		DisablePathNormalizing:   true,

		Dial: fasthttp.Dial,
	}))
	startServer(app, ln)

	proxyTestExpectGet(t, clientpkg.New(), "http://"+addr, "test_local_client")
}

// go test -run Test_Proxy_WithClient_Nil_Panics
func Test_Proxy_WithClient_Nil_Panics(t *testing.T) {
	t.Parallel()

	require.PanicsWithValue(t, "proxy: WithClient requires a non-nil *fasthttp.Client", func() {
		WithClient(nil)
	})
}

// go test -run Test_Proxy_SelectClient_NilGlobal
func Test_Proxy_SelectClient_NilGlobal(t *testing.T) {
	t.Parallel()

	selectedClient, err := selectClient(nil)
	require.ErrorIs(t, err, errNilGlobalProxyClient)
	require.Nil(t, selectedClient)
}

// go test -run Test_Proxy_NilClientOverride_AcrossHelpers
func Test_Proxy_NilClientOverride_AcrossHelpers(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString("proxied")
	})

	tests := map[string]func(c fiber.Ctx) error{
		"Do": func(c fiber.Ctx) error {
			return Do(c, "http://"+addr, nil)
		},
		"DoRedirects": func(c fiber.Ctx) error {
			return DoRedirects(c, "http://"+addr, 1, nil)
		},
		"DoDeadline": func(c fiber.Ctx) error {
			return DoDeadline(c, "http://"+addr, time.Now().Add(time.Second), nil)
		},
		"DoTimeout": func(c fiber.Ctx) error {
			return DoTimeout(c, "http://"+addr, time.Second, nil)
		},
	}

	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Get("/test", run)

			apptest.ExpectBody(t, app, "/test", fiber.StatusInternalServerError, errNilProxyClientOverride.Error())
		})
	}
}

// go test -run Test_Proxy_Domain_Forward_Local
func Test_Proxy_Domain_Forward_Local(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen(fiber.NetworkTCP4, "127.0.0.1:0")
	require.NoError(t, err)
	app := fiber.New()

	// target server
	ln1, err := net.Listen(fiber.NetworkTCP4, "127.0.0.1:0")
	require.NoError(t, err)
	app1 := fiber.New()

	app1.Get("/test", func(c fiber.Ctx) error {
		return c.SendString("test_local_client:" + c.Query("query_test"))
	})

	proxyAddr := ln.Addr().String()
	targetAddr := ln1.Addr().String()
	localDomain := strings.Replace(proxyAddr, "127.0.0.1", "localhost", 1)
	app.Use(DomainForward(localDomain, "http://"+targetAddr, &fasthttp.Client{
		NoDefaultUserAgentHeader: true,
		DisablePathNormalizing:   true,

		Dial: fasthttp.Dial,
	}))
	startServer(app, ln)
	startServer(app1, ln1)

	proxyTestExpectGet(t, clientpkg.New(), "http://"+localDomain+"/test?query_test=true", "test_local_client:true")
}

// go test -run Test_Proxy_Balancer_Forward_Local
func Test_Proxy_Balancer_Forward_Local(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString("forwarded")
	})

	app.Use(BalancerForward([]string{addr}))

	apptest.ExpectBody(t, app, "/", fiber.StatusOK, "forwarded")
}

func Test_Proxy_Balancer_ModifyRequestCanSetXRealIP(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString(c.Get("X-Real-IP"))
	})

	app := fiber.New()
	app.Use(Balancer(Config{
		Servers: []string{addr},
		ModifyRequest: func(c fiber.Ctx) error {
			c.Request().Header.Set("X-Real-IP", "203.0.113.7")
			return nil
		},
	}))

	req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
	req.Host = addr
	req.Header.Set("X-Real-IP", "6.6.6.6")

	resp := apptest.Do(t, app, req)
	require.Equal(t, "203.0.113.7", apptest.Body(t, resp))
}

// go test -run Test_Proxy_ConnectionHeader
func Test_Proxy_ConnectionHeader(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		want string // the Connection header the client gets back
		keep bool   // Config.KeepConnectionHeader
	}{
		{name: "KeepConnectionHeader passes the upstream's through", want: "backend", keep: true},
		{name: "dropped by default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
				c.Set(fiber.HeaderConnection, "backend")
				return c.SendString("ok")
			})

			app := fiber.New()
			app.Use(Balancer(Config{Servers: []string{addr}, KeepConnectionHeader: tc.keep}))

			resp := apptest.Do(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHost(addr),
				apptest.WithHeader(fiber.HeaderConnection, "keep-alive")))
			require.Equal(t, tc.want, resp.Header.Get(fiber.HeaderConnection))
		})
	}
}

// Test_Proxy_DomainForward_HostMatch pins which Host headers a DomainForward
// proxies and which it leaves to the handlers behind it. The upstream answers
// "proxied"; the route behind, when a row has one, answers its fallback.
func Test_Proxy_DomainForward_HostMatch(t *testing.T) {
	t.Parallel()

	type request struct {
		host   string
		body   string // not checked when empty
		status int
	}

	for _, tc := range []struct {
		name     string
		domain   string // the hostname the handler is configured with
		fallback string // what the route behind the handler answers; none is registered when empty
		requests []request
	}{
		// The host gate folds case per RFC 9110 §4.2.3 — an inbound Host header
		// with different casing than the configured hostname must still be
		// proxied, not silently passed through.
		{
			name:     "mixed-case Host is still proxied",
			domain:   "api.example.com",
			requests: []request{{host: "API.Example.com", status: fiber.StatusOK, body: "proxied"}},
		},
		// A Unicode hostname literal — the natural way to write a non-ASCII
		// domain in Go source — still matches the request a real client sends,
		// which is always the Punycode form (RFC 5891). Before this was fixed,
		// the rule silently never matched: every real request for the configured
		// domain fell through unproxied.
		{
			name:     "a Unicode-literal hostname matches the Punycode form on the wire",
			domain:   "münchen.example.com",
			requests: []request{{host: "xn--mnchen-3ya.example.com", status: fiber.StatusOK, body: "proxied"}},
		},
		// Folding preserves an explicit port on the configured hostname, so
		// DomainForward("münchen.example.com:8080", ...) keeps requiring that port
		// rather than silently dropping the restriction.
		{
			name:   "a folded hostname keeps its port",
			domain: "münchen.example.com:8080",
			requests: []request{
				{host: "xn--mnchen-3ya.example.com:8080", status: fiber.StatusOK, body: "proxied"},
				// a different port must still fall through unproxied
				{host: "xn--mnchen-3ya.example.com:9090", status: fiber.StatusNotFound},
			},
		},
		// Construction-time hostname folding keeps the brackets required around an
		// IPv6 literal when it has an explicit port (RFC 3986, section 3.2.2).
		{
			name:     "an IPv6 literal keeps its brackets",
			domain:   "[::1]:8080",
			fallback: "fallthrough",
			requests: []request{{host: "[::1]:8080", status: fiber.StatusOK, body: "proxied"}},
		},
		{
			name:     "a Host that does not match continues to the next handler",
			domain:   "api.example.com",
			fallback: "local",
			requests: []request{
				{host: "api.example.com", status: fiber.StatusOK, body: "proxied"},
				{host: "API.Example.com:8080", status: fiber.StatusOK, body: "proxied"},
				{host: "www.example.com", status: fiber.StatusOK, body: "local"},
				{host: "www.example.com:8080", status: fiber.StatusOK, body: "local"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
				return c.SendString("proxied")
			})

			app := fiber.New()
			app.Use(DomainForward(tc.domain, "http://"+addr))
			if tc.fallback != "" {
				app.Use(func(c fiber.Ctx) error {
					return c.SendString(tc.fallback)
				})
			}

			for _, r := range tc.requests {
				resp := apptest.Do(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHost(r.host)))
				require.Equal(t, r.status, resp.StatusCode, "Host %s", r.host)
				if r.body != "" {
					require.Equal(t, r.body, apptest.Body(t, resp), "Host %s", r.host)
				}
			}
		})
	}
}

// sendRaw parses raw as the request a client put on the wire and runs app's
// handler on it, returning the response body. Reading from the wire is what
// lets a test send a chunked body with a trailer section, which the request
// setters cannot express.
func sendRaw(t *testing.T, app *fiber.App, raw string) string {
	t.Helper()

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	return sendParsed(t, app, req, raw)
}

// sendRawUnnormalized is sendRaw with the header names kept exactly as
// written, the way a front end translating HTTP/2 down to HTTP/1.1 leaves them.
func sendRawUnnormalized(t *testing.T, app *fiber.App, raw string) string {
	t.Helper()

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.DisableNormalizing()
	return sendParsed(t, app, req, raw)
}

// sendParsed reads raw into req and runs app's handler on the result.
func sendParsed(t *testing.T, app *fiber.App, req *fasthttp.Request, raw string) string {
	t.Helper()

	require.NoError(t, req.Read(bufio.NewReader(strings.NewReader(raw))))

	fctx := &fasthttp.RequestCtx{}
	fctx.Init(req, nil, nil)
	app.Handler()(fctx)

	return string(fctx.Response.Body())
}

// loopbackAlias returns addr with its host rewritten to "localhost", naming the
// same listener under a different authority.
//
// A cross-host redirect is decided by comparing the authority as written, not
// by where it resolves, so this is enough to make the hop cross an origin
// without binding a second loopback address.
func loopbackAlias(t *testing.T, addr string) string {
	t.Helper()

	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	return net.JoinHostPort("localhost", port)
}

// matchingFieldLines reports every field line whose name matches one of names,
// in the spelling the header store holds.
func matchingFieldLines(c fiber.Ctx, names ...string) []string {
	var found []string
	for k, v := range c.Request().Header.All() {
		for _, name := range names {
			if utils.EqualFold(utils.UnsafeString(k), name) {
				found = append(found, string(k)+"="+string(v))
			}
		}
	}
	return found
}

// Test_Proxy_HopByHopHeadersStrippedWhateverTheirCase checks that the RFC 9110 §7.6.1 strip holds
// for names in lower case, as HTTP/2 and HTTP/3 require on the wire.
//
// Del matches the stored key byte for byte, so under DisableHeaderNormalizing
// it removed nothing: the hop-by-hop headers reached the upstream, and so did a
// field named in Connection, which is a peer smuggling a connection-scoped
// header through an intermediary required to drop it.
func Test_Proxy_HopByHopHeadersStrippedWhateverTheirCase(t *testing.T) {
	t.Parallel()

	addr, seen := proxyTestFieldLinesUpstream(t, "upstream", "Upgrade", "TE", "Keep-Alive", "Connection", "X-Smuggled")

	app := fiber.New(fiber.Config{DisableHeaderNormalizing: true})
	app.Use(Forward("http://" + addr))

	body := sendRawUnnormalized(t, app, "GET / HTTP/1.1\r\nHost: front\r\n"+
		"connection: x-smuggled\r\n"+
		"x-smuggled: through-the-proxy\r\n"+
		"upgrade: websocket\r\n"+
		"te: trailers\r\n"+
		"keep-alive: timeout=5\r\n\r\n")
	require.Equal(t, "upstream", body)

	require.Empty(t, <-seen, "no hop-by-hop or Connection-listed header may reach the upstream")
}

// Test_Proxy_RealIPReplacedWhateverTheirCase checks that the client cannot keep
// a claim of its own beside the address this hop writes.
func Test_Proxy_RealIPReplacedWhateverTheirCase(t *testing.T) {
	t.Parallel()

	addr, seen := proxyTestFieldLinesUpstream(t, "upstream", realIPHeader)

	app := fiber.New(fiber.Config{DisableHeaderNormalizing: true})
	app.Use(Forward("http://" + addr))

	body := sendRawUnnormalized(t, app, "GET / HTTP/1.1\r\nHost: front\r\n"+
		"x-real-ip: 6.6.6.6\r\n"+
		"X-Real-IP: 7.7.7.7\r\n\r\n")
	require.Equal(t, "upstream", body)

	lines := <-seen
	require.Len(t, lines, 1, "exactly one X-Real-IP reaches the upstream: %v", lines)
	require.NotContains(t, lines[0], "6.6.6.6")
	require.NotContains(t, lines[0], "7.7.7.7")
}

// Test_Proxy_CrossHostCredentialsStrippedWhateverTheirCase checks that a
// redirect leaving the host the caller addressed does not carry the caller's
// credentials to wherever it points.
func Test_Proxy_CrossHostCredentialsStrippedWhateverTheirCase(t *testing.T) {
	t.Parallel()

	finalAddr, seen := proxyTestFieldLinesUpstream(t, "final", fiber.HeaderAuthorization, fiber.HeaderCookie)

	// The redirect has to cross to a different host, and that is decided by
	// comparing the authority as written rather than where it resolves — so
	// name the same listener "localhost" instead of binding a second loopback
	// address. 127.0.0.2 is bound by default only on Linux.
	_, redirectAddr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		c.Location("http://" + loopbackAlias(t, finalAddr) + "/")
		return c.SendStatus(fiber.StatusFound)
	})

	app := fiber.New(fiber.Config{DisableHeaderNormalizing: true})
	app.Use(func(c fiber.Ctx) error {
		return DoRedirects(c, "http://"+redirectAddr, 3)
	})

	body := sendRawUnnormalized(t, app, "GET / HTTP/1.1\r\nHost: front\r\n"+
		"authorization: Bearer caller-token\r\n"+
		"cookie: session=caller\r\n\r\n")
	require.Equal(t, "final", body)

	require.Empty(t, <-seen, "credentials must not follow a redirect off the host they were sent to")
}

// Test_Proxy_CrossHostStripsEveryCredentialHeader checks that the set dropped
// on a redirect off the addressed host is the one the client package drops:
// Cookie2 carries a session exactly as Cookie does, and a list that covers one
// but not the other leaks through the gap.
func Test_Proxy_CrossHostStripsEveryCredentialHeader(t *testing.T) {
	t.Parallel()

	finalAddr, seen := proxyTestFieldLinesUpstream(t, "final",
		fiber.HeaderAuthorization,
		fiber.HeaderProxyAuthorization,
		fiber.HeaderProxyAuthenticate,
		fiber.HeaderWWWAuthenticate,
		fiber.HeaderCookie,
		"Cookie2",
	)

	_, redirectAddr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		c.Location("http://" + loopbackAlias(t, finalAddr) + "/")
		return c.SendStatus(fiber.StatusFound)
	})

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		return DoRedirects(c, "http://"+redirectAddr, 3)
	})

	req := apptest.Req(fiber.MethodGet, "/",
		apptest.WithHeader(fiber.HeaderAuthorization, "Bearer caller-token"),
		apptest.WithHeader(fiber.HeaderProxyAuthorization, "Basic proxy"),
		apptest.WithHeader(fiber.HeaderProxyAuthenticate, "Basic realm=x"),
		apptest.WithHeader(fiber.HeaderWWWAuthenticate, "Basic realm=y"),
		apptest.WithHeader(fiber.HeaderCookie, "session=caller"),
		apptest.WithHeader("Cookie2", `$Version="1"`))

	apptest.Expect(t, app, req, fiber.StatusOK)

	require.Empty(t, <-seen, "no credential header may follow a redirect off the addressed host")
}

// Test_Proxy_ResponseStripIgnoresTheAppsNormalizationSetting checks the
// response-side hop-by-hop strip against a proxy whose outbound client
// preserves upstream header casing while the app itself normalizes.
//
// The two settings are independent — the client stamps its own onto the
// response header on every hop — so reading the app's told the strip nothing
// about the keys it was about to match, and it removed nothing: the upstream's
// connection-scoped headers reached the client unchanged.
func Test_Proxy_ResponseStripIgnoresTheAppsNormalizationSetting(t *testing.T) {
	t.Parallel()

	// The upstream preserves the casing it writes, and writes its hop-by-hop
	// headers in lower case.
	upstream := fiber.New(fiber.Config{DisableHeaderNormalizing: true})
	upstream.Get("/", func(c fiber.Ctx) error {
		c.Response().Header.Set("keep-alive", "timeout=5")
		c.Response().Header.Set("upgrade", "websocket")
		c.Response().Header.Set("te", "trailers")
		return c.SendString("upstream")
	})
	ln, err := net.Listen(fiber.NetworkTCP4, "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	startServer(upstream, ln)

	cli := &fasthttp.Client{DisableHeaderNamesNormalizing: true}

	// ...while the app takes the default, so the two settings disagree.
	app := fiber.New()
	app.Use(Forward("http://"+addr, cli))

	resp := apptest.Expect(t, app, "/", fiber.StatusOK)

	for _, name := range []string{fiber.HeaderKeepAlive, fiber.HeaderUpgrade, fiber.HeaderTE} {
		require.Empty(t, resp.Header.Values(name),
			"%s must not reach the client whatever case the upstream wrote it in", name)
	}
}

func Test_Proxy_Forward_RestoresHost(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString("forwarded")
	})

	app := fiber.New()
	seen := make(chan string, 1)
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		seen <- c.Hostname()
		return err
	})
	app.Use(Forward("http://" + addr))

	req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
	req.Host = "public.example.com"
	apptest.Expect(t, app, req, fiber.StatusOK)
	require.Equal(t, "public.example.com", <-seen)
}

func Test_Proxy_HostWithoutPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, out string
	}{
		{in: "example.com", out: "example.com"},
		{in: "example.com:8080", out: "example.com"},
		{in: "[::1]:8080", out: "[::1]"},
		{in: "[::1]", out: "[::1]"},
		{in: "[::1", out: "[::1"},
		{in: "::1", out: "::1"},
	}

	for _, tc := range tests {
		require.Equal(t, tc.out, hostWithoutPort(tc.in), "in=%q", tc.in)
	}
}

// go test -run Test_Proxy_NewHostClient
func Test_Proxy_NewHostClient(t *testing.T) {
	t.Parallel()

	cfg := Config{
		MaxConnsPerHost:     7,
		ReadBufferSize:      2048,
		WriteBufferSize:     3072,
		MaxResponseBodySize: 4096,
		DialDualStack:       true,
	}
	strict := DefaultSecurityPolicy()
	relaxed := DefaultSecurityPolicy()
	relaxed.AllowPrivateIPs = true

	t.Run("carries the config", func(t *testing.T) {
		t.Parallel()

		hc := newHostClient(mustParseTestURL(t, "http://upstream.example:8080"), &cfg, strict)
		require.Equal(t, "upstream.example:8080", hc.Addr)
		require.False(t, hc.IsTLS)
		require.Equal(t, 7, hc.MaxConns)
		require.Equal(t, 2048, hc.ReadBufferSize)
		require.Equal(t, 3072, hc.WriteBufferSize)
		require.Equal(t, 4096, hc.MaxResponseBodySize)
		require.True(t, hc.DialDualStack)
		require.True(t, hc.DisablePathNormalizing)
		require.True(t, hc.NoDefaultUserAgentHeader)
	})

	t.Run("https gets TLS with a floor", func(t *testing.T) {
		t.Parallel()

		hc := newHostClient(mustParseTestURL(t, "https://upstream.example"), &cfg, strict)
		require.True(t, hc.IsTLS)
		require.NotNil(t, hc.TLSConfig)
		require.Equal(t, uint16(tls.VersionTLS12), hc.TLSConfig.MinVersion)
	})

	t.Run("guards the dial unless private IPs are allowed", func(t *testing.T) {
		t.Parallel()

		guarded := newHostClient(mustParseTestURL(t, "http://upstream.example"), &cfg, strict)
		require.NotNil(t, guarded.Dial)
		_, err := guarded.Dial("127.0.0.1:80")
		require.ErrorIs(t, err, ErrUpstreamHostBlocked)

		open := newHostClient(mustParseTestURL(t, "http://upstream.example"), &cfg, relaxed)
		require.Nil(t, open.Dial)
		require.Nil(t, open.DialTimeout)
	})

	// fasthttp hands the timeout a request leaves for the connect only to a DialTimeout.
	t.Run("guards the dial timeout too", func(t *testing.T) {
		t.Parallel()

		guarded := newHostClient(mustParseTestURL(t, "http://upstream.example"), &cfg, strict)
		require.NotNil(t, guarded.DialTimeout)
		for _, timeout := range []time.Duration{0, time.Second} {
			_, err := guarded.DialTimeout("127.0.0.1:80", timeout)
			require.ErrorIs(t, err, ErrUpstreamHostBlocked, "timeout %v", timeout)
		}
	})

	// Only fasthttp's own dialer adds the port, and the guard's does not.
	t.Run("adds the port its scheme defaults to", func(t *testing.T) {
		t.Parallel()

		for raw, want := range map[string]string{
			"http://upstream.example":           "upstream.example:80",
			"https://upstream.example":          "upstream.example:443",
			"http://upstream.example:8080":      "upstream.example:8080",
			"https://upstream.example:8443":     "upstream.example:8443",
			"http://[2001:db8::1]":              "[2001:db8::1]:80",
			"https://[2001:db8::1]":             "[2001:db8::1]:443",
			"http://[2001:db8::1]:8080":         "[2001:db8::1]:8080",
			"http://203.0.113.5":                "203.0.113.5:80",
			"https://upstream.example:443":      "upstream.example:443",
			"http://upstream.example:80":        "upstream.example:80",
			"https://[2001:db8::1]:8443":        "[2001:db8::1]:8443",
			"http://upstream.example.:8080":     "upstream.example.:8080",
			"http://xn--bcher-kva.example/":     "xn--bcher-kva.example:80",
			"https://xn--bcher-kva.example:444": "xn--bcher-kva.example:444",
		} {
			hc := newHostClient(mustParseTestURL(t, raw), &cfg, strict)
			require.Equal(t, want, hc.Addr, raw)
		}
	})
}

// go test -run Test_Proxy_Balancer_MaxResponseBodySize
func Test_Proxy_Balancer_MaxResponseBodySize(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString(strings.Repeat("x", 64))
	})

	for name, tc := range map[string]struct {
		limit   int
		wantErr bool
	}{
		"over the limit":   {limit: 16, wantErr: true},
		"within the limit": {limit: 128},
		"unbounded":        {limit: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var handled error
			app := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error {
				handled = err
				return c.SendStatus(fiber.StatusBadGateway)
			}})
			app.Use(Balancer(Config{Servers: []string{addr}, MaxResponseBodySize: tc.limit}))

			resp := apptest.Do(t, app, "/")
			if tc.wantErr {
				require.ErrorIs(t, handled, fasthttp.ErrBodyTooLarge)
				require.Equal(t, fiber.StatusBadGateway, resp.StatusCode)
				return
			}
			require.NoError(t, handled)
			require.Equal(t, fiber.StatusOK, resp.StatusCode)
		})
	}
}

// go test -run Test_Proxy_Balancer_WriteBufferSize
//
// Headers larger than WriteBufferSize must still arrive whole.
func Test_Proxy_Balancer_WriteBufferSize(t *testing.T) {
	t.Parallel()

	_, addr := createProxyTestServerIPv4(t, func(c fiber.Ctx) error {
		return c.SendString(strconv.Itoa(len(c.Get(fiber.HeaderCookie))))
	})

	app := fiber.New()
	app.Use(Balancer(Config{Servers: []string{addr}, WriteBufferSize: 256}))

	cookie := "session=" + strings.Repeat("a", 2000)
	req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
	req.Header.Set(fiber.HeaderCookie, cookie)

	resp := apptest.Do(t, app, req)
	require.Equal(t, strconv.Itoa(len(cookie)), apptest.Body(t, resp))
}
