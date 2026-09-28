package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
)

func newWebFetchTool(t *testing.T) *webFetchTool {
	t.Helper()
	return &webFetchTool{client: newSafeHTTPClient(5*time.Second, productionBlockedAddr), maxBytes: 1 << 20}
}

// TestIsBlockedAddr_Matrix is the SSRF address matrix: every address a real
// request must never reach, plus a normal public IP that must still be
// allowed through (guarding against over-blocking).
func TestIsBlockedAddr_Matrix(t *testing.T) {
	cases := []struct {
		addr    string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"169.254.169.254", true}, // cloud metadata, link-local range
		{"10.0.0.1", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"::ffff:127.0.0.1", true}, // IPv4-mapped IPv6
		{"fe80::1", true},          // IPv6 link-local
		{"fc00::1", true},          // IPv6 ULA / "private"
		{"::", true},               // unspecified
		{"172.16.0.5", true},
		{"192.168.1.1", true},
		{"100.64.0.1", true}, // CGNAT
		{"224.0.0.1", true},  // multicast
		{"ff02::1", true},    // IPv6 multicast
		{"8.8.8.8", false},
		{"93.184.216.34", false}, // example.com-ish public IP
	}
	for _, c := range cases {
		addr, err := netip.ParseAddr(c.addr)
		require.NoError(t, err, c.addr)
		assert.Equal(t, c.blocked, isBlockedAddr(addr), "address %s", c.addr)
	}
}

// TestIsBlockedAddr_ResidualRanges covers the ranges added on top of the
// matrix above: the reserved 240.0.0.0/4 block (and its own top address,
// the limited broadcast address), the two remaining IETF-reserved IPv4
// blocks, and IPv6 encodings (NAT64, 6to4, IPv4-compatible) that embed a
// blocked or an ordinary public IPv4 address.
func TestIsBlockedAddr_ResidualRanges(t *testing.T) {
	cases := []struct {
		addr    string
		blocked bool
	}{
		{"255.255.255.255", true},   // limited broadcast, within 240.0.0.0/4
		{"240.0.0.1", true},         // 240.0.0.0/4 reserved ("Class E")
		{"192.0.0.1", true},         // 192.0.0.0/24 IETF protocol assignments
		{"192.0.0.170", true},       // NAT64/DNS64 discovery address within that block
		{"198.18.0.1", true},        // 198.18.0.0/15 benchmarking
		{"198.19.255.255", true},    // top of the same /15
		{"64:ff9b::7f00:1", true},   // NAT64-embedded 127.0.0.1
		{"64:ff9b::808:808", false}, // NAT64-embedded 8.8.8.8 (public) must not be blocked
		{"2002:7f00:1::", true},     // 6to4-embedded 127.0.0.1
		{"2002:0808:0808::", false}, // 6to4-embedded 8.8.8.8 (public) must not be blocked
		{"::10.0.0.1", true},        // IPv4-compatible-embedded 10.0.0.1 (private)
		{"::93.184.216.34", false},  // IPv4-compatible-embedded public IP must not be blocked
	}
	for _, c := range cases {
		addr, err := netip.ParseAddr(c.addr)
		require.NoError(t, err, c.addr)
		assert.Equal(t, c.blocked, isBlockedAddr(addr), "address %s", c.addr)
	}
}

func TestWebFetch_RefusesLoopbackTarget(t *testing.T) {
	w := newWebFetchTool(t)
	out, err := w.run(context.Background(), mustArgs(t, webFetchArgs{URL: "http://127.0.0.1:1/"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "web_fetch:")
}

// TestWebFetch_HostnameResolvingToPrivateIPRefused covers a hostname
// resolving to a private IP using "localhost", which every OS resolves
// locally (via /etc/hosts or its NSS equivalent) with no real DNS query, so
// the test needs no network access. The point
// is that Control receives the *resolved* address (127.0.0.1 or ::1), not
// the hostname text, so a pre-resolution string check on "localhost" would
// have been unnecessary and a check against the wrong thing would have
// missed this case entirely.
func TestWebFetch_HostnameResolvingToPrivateIPRefused(t *testing.T) {
	w := newWebFetchTool(t)
	out, err := w.run(context.Background(), mustArgs(t, webFetchArgs{URL: "http://localhost:1/"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "web_fetch:")
}

func TestWebFetch_RefusesUnsupportedScheme(t *testing.T) {
	w := newWebFetchTool(t)
	out, err := w.run(context.Background(), mustArgs(t, webFetchArgs{URL: "ftp://example.com/file"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "unsupported scheme")
}

func TestWebFetch_RefusesEmptyURL(t *testing.T) {
	w := newWebFetchTool(t)
	out, err := w.run(context.Background(), mustArgs(t, webFetchArgs{URL: ""}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "must not be empty")
}

func TestWebFetch_InvalidArgsReturnsResultString(t *testing.T) {
	w := newWebFetchTool(t)
	out, err := w.run(context.Background(), json.RawMessage(`{bad`), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "invalid arguments")
}

// TestWebFetch_PublicURLSucceeds proves the guard does not over-block: a
// loopback httptest server dialed via 127.0.0.1 is of course itself
// blocked, so this test instead drives the full success path (HTML
// stripping, untrusted-content note) directly against an httptest server,
// while a separate opt-in end-to-end test covers a genuine public URL.
func TestWebFetch_FetchesAndStripsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><style>.x{color:red}</style><script>alert(1)</script></head><body><h1>Hello</h1><p>World &amp; friends</p></body></html>`))
	}))
	defer srv.Close()

	// The httptest server binds to loopback, which controlRejectUnsafeAddr
	// would otherwise refuse - build a client without the SSRF dialer for
	// this test so it exercises the HTML-to-text and untrusted-content-note
	// behavior instead of the address guard (covered separately above).
	w := &webFetchTool{client: srv.Client(), maxBytes: 1 << 20}

	out, err := w.run(context.Background(), mustArgs(t, webFetchArgs{URL: srv.URL}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "Hello")
	assert.Contains(t, out, "World & friends")
	assert.NotContains(t, out, "alert(1)")
	assert.NotContains(t, out, "color:red")
	assert.NotContains(t, out, "<h1>")
	assert.Contains(t, out, "untrusted third-party text")
}

func TestWebFetch_SkipsNonTextContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a})
	}))
	defer srv.Close()

	wf := &webFetchTool{client: srv.Client(), maxBytes: 1 << 20}
	out, err := wf.run(context.Background(), mustArgs(t, webFetchArgs{URL: srv.URL}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "binary content skipped")
	assert.Contains(t, out, "image/png")
}

func TestWebFetch_AllowsJSONContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))
	defer srv.Close()

	wf := &webFetchTool{client: srv.Client(), maxBytes: 1 << 20}
	out, err := wf.run(context.Background(), mustArgs(t, webFetchArgs{URL: srv.URL}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, `"hello":"world"`)
	assert.NotContains(t, out, "binary content skipped")
}

func TestWebFetch_AllowsXMLAndSuffixedAndJavaScriptContentTypes(t *testing.T) {
	cases := []struct {
		contentType string
		body        string
	}{
		{"application/xml", "<root>hello</root>"},
		{"application/vnd.api+json; charset=utf-8", `{"hello":"world"}`},
		{"application/atom+xml", "<feed>hello</feed>"},
		{"application/javascript", "console.log('hello')"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", c.contentType)
			_, _ = w.Write([]byte(c.body))
		}))

		wf := &webFetchTool{client: srv.Client(), maxBytes: 1 << 20}
		out, err := wf.run(context.Background(), mustArgs(t, webFetchArgs{URL: srv.URL}), agent.Meta{})
		require.NoError(t, err)
		assert.NotContains(t, out, "binary content skipped", "content-type: %q", c.contentType)
		assert.Contains(t, out, "hello", "content-type: %q", c.contentType)

		srv.Close()
	}
}

// TestWebFetch_NonHTMLTextualTypesAreReturnedVerbatim proves htmlToText only
// ever runs on text/html and application/xhtml+xml: every other textual
// type must come back byte-for-byte, tags and entities untouched, since
// stripping "<...>" out of source code or XML/JSON containing "<" would
// corrupt it. Asserting the literal body substring (not just that some
// expected word survives) is what actually exercises this - a body whose
// tags were wrongly stripped would still contain the word "hello" as loose
// text, which would not catch the regression a weaker assertion missed.
func TestWebFetch_NonHTMLTextualTypesAreReturnedVerbatim(t *testing.T) {
	cases := []struct {
		contentType string
		body        string
	}{
		{"text/plain", `if (a < b && c > d) { x = "&amp;" }`},
		{"application/xml", "<root><item>hello</item></root>"},
		{"application/json", `{"a": "<b>not html</b>"}`},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", c.contentType)
			_, _ = w.Write([]byte(c.body))
		}))

		wf := &webFetchTool{client: srv.Client(), maxBytes: 1 << 20}
		out, err := wf.run(context.Background(), mustArgs(t, webFetchArgs{URL: srv.URL}), agent.Meta{})
		require.NoError(t, err)
		assert.Contains(t, out, c.body, "content-type: %q", c.contentType)

		srv.Close()
	}
}

// TestWebFetch_HTMLContentTypeStillStripsTags is the html/xhtml half of the
// same contract: those two types must still go through htmlToText.
func TestWebFetch_HTMLContentTypeStillStripsTags(t *testing.T) {
	for _, contentType := range []string{"text/html", "application/xhtml+xml"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write([]byte("<p>hello</p>"))
		}))

		wf := &webFetchTool{client: srv.Client(), maxBytes: 1 << 20}
		out, err := wf.run(context.Background(), mustArgs(t, webFetchArgs{URL: srv.URL}), agent.Meta{})
		require.NoError(t, err)
		assert.Contains(t, out, "hello", "content-type: %q", contentType)
		assert.NotContains(t, out, "<p>", "content-type: %q", contentType)

		srv.Close()
	}
}

func TestWebFetch_MaxBytesCapTruncates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 100)))
	}))
	defer srv.Close()

	wf := &webFetchTool{client: srv.Client(), maxBytes: 10}
	out, err := wf.run(context.Background(), mustArgs(t, webFetchArgs{URL: srv.URL}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "[truncated to 10 bytes]")
}

// TestWebFetch_FollowsRedirectThenRefusesDifferentTarget proves web_fetch
// follows a redirect through the real production client (CheckRedirect
// deciding to follow, then Dialer.Control firing again on the resulting
// dial) rather than bypassing newSafeHTTPClient with srv.Client() the way
// the tests above do for unrelated (content-parsing) coverage. Both
// httptest servers bind to loopback, so isBlockedAddr alone cannot tell
// them apart; the injected predicate (see newSafeHTTPClient) allows only
// the redirecting server's own port and blocks everything else, including
// the redirect target's port on that same loopback address - the real
// Control hook then refuses the second hop exactly the way isBlockedAddr
// would refuse a private/loopback redirect target in production, and the
// hit counters prove both that the first hop was actually dialed and that
// the second one never was.
func TestWebFetch_FollowsRedirectThenRefusesDifferentTarget(t *testing.T) {
	var targetHits, redirectHits int32

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		_, _ = w.Write([]byte("should never be reached"))
	}))
	defer target.Close()

	var redirectSrv *httptest.Server
	redirectSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&redirectHits, 1)
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirectSrv.Close()

	redirectPort := redirectSrv.Listener.Addr().(*net.TCPAddr).Port
	client := newSafeHTTPClient(5*time.Second, func(ap netip.AddrPort) bool {
		return int(ap.Port()) != redirectPort
	})
	w := &webFetchTool{client: client, maxBytes: 1 << 20}

	out, err := w.run(context.Background(), mustArgs(t, webFetchArgs{URL: redirectSrv.URL}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "web_fetch:")
	assert.NotContains(t, out, "should never be reached")
	assert.Equal(t, int32(1), atomic.LoadInt32(&redirectHits), "the first hop must actually be dialed")
	assert.Equal(t, int32(0), atomic.LoadInt32(&targetHits), "the redirect target must never be reached")
}

// TestWebFetch_TooManyRedirectsRefused proves maxRedirects is enforced by
// the real CheckRedirect, using a predicate that blocks nothing (this test
// is only about the redirect count, not address safety) so every hop is
// actually dialed and the hit counter proves exactly how many were.
func TestWebFetch_TooManyRedirectsRefused(t *testing.T) {
	var target *httptest.Server
	var hits int32
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, target.URL+"/next", http.StatusFound)
	}))
	defer target.Close()

	client := newSafeHTTPClient(5*time.Second, func(netip.AddrPort) bool { return false })
	w := &webFetchTool{client: client, maxBytes: 1 << 20}

	out, err := w.run(context.Background(), mustArgs(t, webFetchArgs{URL: target.URL}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, fmt.Sprintf("stopped after %d redirects", maxRedirects))
	assert.Equal(t, int32(maxRedirects), atomic.LoadInt32(&hits), "must dial the original request plus exactly maxRedirects-1 follow-ups before refusing")
}

// TestWebFetch_PublicURLE2E is gated behind MTCLAW_E2E=1: it requires real
// internet access, which CI and the default local run must not depend on.
func TestWebFetch_PublicURLE2E(t *testing.T) {
	if os.Getenv("MTCLAW_E2E") != "1" {
		t.Skip("set MTCLAW_E2E=1 to run tests that require real internet access")
	}
	w := newWebFetchTool(t)
	out, err := w.run(context.Background(), mustArgs(t, webFetchArgs{URL: "https://example.com"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "HTTP 200")
}
