package proxy

import (
	"errors"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// stubConn satisfies net.Conn so dialValidatedIPs can return a non-nil
// successful dial without opening real sockets.
type stubConn struct{ addr string }

func (stubConn) Read([]byte) (int, error)         { return 0, nil }
func (stubConn) Write(b []byte) (int, error)      { return len(b), nil }
func (stubConn) Close() error                     { return nil }
func (stubConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (stubConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (stubConn) SetDeadline(time.Time) error      { return nil }
func (stubConn) SetReadDeadline(time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(time.Time) error { return nil }

func Test_Coverage_ResolveAndValidateHost_IPLiteralOK(t *testing.T) {
	t.Parallel()
	ips, err := resolveAndValidateHost("8.8.8.8", time.Time{})
	require.NoError(t, err)
	require.Len(t, ips, 1)
	require.True(t, ips[0].Equal(net.ParseIP("8.8.8.8")))
}

func Test_Coverage_ResolveAndValidateHost_BlocksIPLiteral(t *testing.T) {
	t.Parallel()
	_, err := resolveAndValidateHost("127.0.0.1", time.Time{})
	require.ErrorIs(t, err, ErrUpstreamHostBlocked)
}

// Test_Coverage_ResolveAndValidateHost_LookupFails covers the
// LookupIPAddr error wrapping branch using a reserved RFC 6761 TLD that
// never resolves and therefore never depends on outbound DNS.
func Test_Coverage_ResolveAndValidateHost_LookupFails(t *testing.T) {
	t.Parallel()
	_, err := resolveAndValidateHost("definitely-not-a-real-host.invalid", time.Time{})
	require.ErrorIs(t, err, ErrUpstreamHostBlocked)
}

func Test_Coverage_DialValidatedIPs_SuccessOnIPv4(t *testing.T) {
	t.Parallel()
	called := atomic.Bool{}
	guard := dialGuard{dial: func(network, addr string) (net.Conn, error) {
		called.Store(true)
		require.Equal(t, "tcp", network)
		require.Equal(t, "203.0.113.1:80", addr)
		return stubConn{addr: addr}, nil
	}}
	conn, err := guard.dialValidatedIPs([]net.IP{net.ParseIP("203.0.113.1")}, "example.test", "80")
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.True(t, called.Load())
}

// Test_Coverage_DialValidatedIPs_SkipsV6_WhenNoDualStack covers the IPv4-only branch: an IPv6
// candidate is skipped before any dial, and the error does not match ErrUpstreamHostBlocked, since
// nothing was blocked.
func Test_Coverage_DialValidatedIPs_SkipsV6_WhenNoDualStack(t *testing.T) {
	t.Parallel()
	called := atomic.Bool{}
	guard := dialGuard{dial: func(string, string) (net.Conn, error) {
		called.Store(true)
		return nil, errors.New("should not dial")
	}}
	_, err := guard.dialValidatedIPs([]net.IP{net.ParseIP("2606:4700:4700::1111")}, "example.test", "80")
	require.ErrorIs(t, err, errIPv6Only)
	require.NotErrorIs(t, err, ErrUpstreamHostBlocked)
	require.NotErrorIs(t, err, ErrUpstreamHostUnresolvable)
	require.ErrorContains(t, err, "example.test has only IPv6 addresses, and DialDualStack is off")
	require.False(t, called.Load(), "IPv6 candidate must be skipped without dialing")
}

// Test_Coverage_DialValidatedIPs_FallsBackOnError exercises the
// "remember last error, continue to next IP" branch and the final
// "return lastErr" path.
func Test_Coverage_DialValidatedIPs_FallsBackOnError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("dial-refused")
	var attempts atomic.Int32
	guard := dialGuard{dial: func(string, string) (net.Conn, error) {
		attempts.Add(1)
		return nil, sentinel
	}}
	_, err := guard.dialValidatedIPs([]net.IP{net.ParseIP("203.0.113.1"), net.ParseIP("203.0.113.2")}, "example.test", "80")
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, int32(2), attempts.Load())
}

func Test_Coverage_DialValidatedIPs_DualStackTriesV6(t *testing.T) {
	t.Parallel()
	var addr string
	guard := dialGuard{dualStack: true, dial: func(_, a string) (net.Conn, error) {
		addr = a
		return stubConn{addr: a}, nil
	}}
	_, err := guard.dialValidatedIPs([]net.IP{net.ParseIP("2606:4700:4700::1111")}, "example.test", "443")
	require.NoError(t, err)
	require.Equal(t, "[2606:4700:4700::1111]:443", addr)
}

// Test_Coverage_DialChecked_PassesGuardThenDials covers the post-guard
// dial dispatch (the final return inside dialGuard.dialChecked). TEST-NET-1
// (192.0.2.0/24, RFC 5737) is reserved for documentation and not in the
// blocked ranges, so it survives the SSRF check; the dial itself fails
// fast because the kernel has no route. We only assert that an error
// surfaced — the *type* of error depends on the host's networking stack.
func Test_Coverage_DialChecked_PassesGuardThenDials(t *testing.T) {
	t.Parallel()
	dial := newDialGuard(true).dialChecked // dualStack so the dial loop is reached
	_, err := dial("192.0.2.1:1")
	require.Error(t, err)
}

// Test_Coverage_ValidateHostForSSRF_PublicHostname drives the
// DNS-resolution success path of validateHostForSSRF. one.one.one.one is
// chosen because Cloudflare keeps it pointing at 1.1.1.1/1.0.0.1, both
// public. We let validateHostForSSRF do the single resolution itself
// and only skip when the failure was a DNS lookup error — using
// LookupHost as a separate skip-gate would do a redundant lookup and
// could disagree with the second lookup in restricted CI.
func Test_Coverage_ValidateHostForSSRF_PublicHostname(t *testing.T) {
	t.Parallel()
	err := validateHostForSSRF("one.one.one.one", nil)
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		t.Skipf("offline DNS, skipping: %v", err)
	}
	require.NoError(t, err)
}

// Test_Coverage_ConfigDefault_FillsDefaults checks the defaults for a Config with Servers or a Client.
func Test_Coverage_ConfigDefault_FillsDefaults(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]Config{
		"servers": {Servers: []string{"http://upstream.example"}},
		"client":  {Client: &fasthttp.LBClient{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := configDefault(in)
			require.Equal(t, ConfigDefault.Timeout, cfg.Timeout)
			require.Equal(t, ConfigDefault.MaxConnsPerHost, cfg.MaxConnsPerHost)
		})
	}
}

// Test_Coverage_DialValidatedIPs_SkipsV6ThenDialsV4 verifies the
// continue branch with a follow-up successful dial: an IPv6 candidate is
// skipped (DialDualStack=false), then an IPv4 candidate dials cleanly.
func Test_Coverage_DialValidatedIPs_SkipsV6ThenDialsV4(t *testing.T) {
	t.Parallel()
	var dialed string
	guard := dialGuard{dial: func(_, addr string) (net.Conn, error) {
		dialed = addr
		return stubConn{addr: addr}, nil
	}}
	conn, err := guard.dialValidatedIPs([]net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("203.0.113.5")}, "example.test", "443")
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.Equal(t, "203.0.113.5:443", dialed)
}

// Test_Coverage_JoinUpstreamPath_RawPathFallback exercises the
// `parsedRaw == "" → parsed.Path` fallback inside the RawPath join when
// the base has a RawPath but the request path does not.
func Test_Coverage_JoinUpstreamPath_RawPathFallback(t *testing.T) {
	t.Parallel()
	base, err := url.Parse("http://upstream.example/a%2Fb")
	require.NoError(t, err)
	require.NotEmpty(t, base.RawPath, "test setup: base must have a RawPath")
	out := joinUpstreamPath(base, "/plain")
	// Plain segment had no escaped chars so its RawPath is empty; the
	// fallback must use parsed.Path verbatim while preserving the base's
	// escaped prefix.
	require.Contains(t, out, "/a%2Fb/plain")
}

// Test_Coverage_CanFastJoinPath_RepeatedSeparator covers a second '?' or '#' left to the url.Parse path.
func Test_Coverage_CanFastJoinPath_RepeatedSeparator(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"/a?b?c", "/a?b#c", "/a#b?c", "/a#b#c"} {
		require.False(t, canFastJoinPath(target), target)
	}
	require.True(t, canFastJoinPath("/a?b"))

	base := mustParseTestURL(t, "http://upstream.example")
	require.Equal(t, "http://upstream.example/a?b?c", joinUpstreamPath(base, "/a?b?c"))
	require.Equal(t, "http://upstream.example/a?b#c", joinUpstreamPath(base, "/a?b#c"))
}
