package netproxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestIPv6LiteralRecoversAfterEarlyClose(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no ::1:", err)
	}
	var calls atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		io.WriteString(w, "healthy")
	}))
	srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	defer srv.Close()
	t.Cleanup(func() { v4Only.Lock(); v4Only.at = nil; v4Only.Unlock() })
	base := &http.Transport{}
	defer base.CloseIdleConnections()
	c := &http.Client{Transport: Dispatch(base)}
	if res, err := c.Get(srv.URL); err == nil {
		res.Body.Close()
		t.Fatal("first request should fail when the peer closes without a reply")
	}
	res, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("IPv6 literal must remain reachable after an early close: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK || string(body) != "healthy" {
		t.Fatalf("recovery response: status=%d body=%q err=%v", res.StatusCode, body, err)
	}
}

// A host whose IPv6 address takes the connection and closes it at once
// (a TUN that can't carry IPv6) is reached over IPv4, the first request
// included, and dialed over IPv4 from then on.
func TestIPv4Fallback(t *testing.T) {
	v6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no ::1:", err)
	}
	defer v6.Close()
	go func() {
		for {
			c, err := v6.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		io.WriteString(w, r.Method+" "+string(b))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	t.Cleanup(func() { v4Only.Lock(); v4Only.at = nil; v4Only.Unlock() })

	var mu sync.Mutex
	var dials []string
	var d net.Dialer
	base := &http.Transport{
		Proxy:             func(*http.Request) (*url.URL, error) { return nil, nil },
		TLSClientConfig:   srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
		ForceAttemptHTTP2: true,
		// example.com resolves to [::1] first, as a dual-stack name does
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			dials = append(dials, network)
			mu.Unlock()
			if network == "tcp4" {
				return d.DialContext(ctx, "tcp4", srv.Listener.Addr().String())
			}
			return d.DialContext(ctx, "tcp", v6.Addr().String())
		},
	}
	c := &http.Client{Transport: Dispatch(base)}
	for i, method := range []string{"GET", "POST"} {
		req, _ := http.NewRequest(method, "https://example.com/", strings.NewReader("body"))
		res, err := c.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v (dials %v)", i, err, dials)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(b) != method+" body" || res.ProtoMajor != 2 {
			t.Fatalf("request %d: %q over %s", i, b, res.Proto)
		}
		base.CloseIdleConnections()
	}
	if got := strings.Join(dials, ","); got != "tcp,tcp4,tcp4" {
		t.Fatalf("dials %s, want tcp,tcp4,tcp4", got)
	}
}
