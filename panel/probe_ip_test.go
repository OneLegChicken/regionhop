package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
)

func writeFileForTest(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }

func TestParseIPEchoResponse(t *testing.T) {
	ok := func(body string) string { return "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\n" + body }
	cases := []struct{ name, in, want string }{
		{"ipv4", ok("203.0.113.7"), "203.0.113.7"},
		{"ipv4 newline", ok("203.0.113.7\n"), "203.0.113.7"},
		{"ipv6 canonicalised", ok("2001:0db8:0000:0000:0000:0000:0000:0001"), "2001:db8::1"},
		{"chunked", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nb\r\n203.0.113.7\n\r\n0\r\n\r\n", "203.0.113.7"},
		{"html is rejected", ok("<script>alert(1)</script>"), ""},
		{"attribute injection is rejected", ok(`1.2.3.4" onerror="x`), ""},
		{"text around an ip is rejected", ok("your ip is 203.0.113.7"), ""},
		{"empty body", ok(""), ""},
		{"not found", "HTTP/1.1 404 Not Found\r\n\r\n203.0.113.7", ""},
		{"blocked page", "HTTP/1.1 403 Forbidden\r\n\r\n203.0.113.7", ""},
		{"no headers", "203.0.113.7", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := parseIPEchoResponse(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// fakeSocks is a no-auth SOCKS5 server for tests. Connections to the host in
// "route" go to the mapped local address; any other destination is accepted
// and then closed, which is enough for the latency probes to see "connected".
func fakeSocks(t *testing.T, route map[string]string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen:", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 262)
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				io.ReadFull(c, buf[:int(buf[1])])
				c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, buf[:5]); err != nil || buf[3] != 3 {
					return
				}
				n := int(buf[4])
				if _, err := io.ReadFull(c, buf[:n+2]); err != nil {
					return
				}
				host := string(buf[:n])
				port := int(buf[n])<<8 | int(buf[n+1])
				target, routed := route[host+":"+strconv.Itoa(port)]
				if !routed {
					c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				done := make(chan struct{}, 2)
				go func() { io.Copy(up, c); done <- struct{}{} }()
				go func() { io.Copy(c, up); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	return l.Addr().String()
}

func withIPTargets(t *testing.T, targets []ipLookupTarget) {
	old := ipLookupTargets
	ipLookupTargets = targets
	t.Cleanup(func() { ipLookupTargets = old })
}

func TestFetchExitIPThroughSocks(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "198.51.100.23\n")
	}))
	defer web.Close()
	socks := fakeSocks(t, map[string]string{"ip.test:80": web.Listener.Addr().String()})

	withIPTargets(t, []ipLookupTarget{{"ip.test", 80, false}})
	if got := fetchExitIP(socks); got != "198.51.100.23" {
		t.Fatalf("got %q", got)
	}
}

func TestFetchExitIPFallsBackToSecondService(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "192.0.2.55") }))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "blocked", 403) }))
	defer bad.Close()
	socks := fakeSocks(t, map[string]string{
		"first.test:80":  bad.Listener.Addr().String(),
		"second.test:80": good.Listener.Addr().String(),
	})
	withIPTargets(t, []ipLookupTarget{{"first.test", 80, false}, {"second.test", 80, false}})
	if got := fetchExitIP(socks); got != "192.0.2.55" {
		t.Fatalf("got %q", got)
	}
}

func TestFetchExitIPRejectsHostileBody(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<img src=x onerror=alert(1)>`)
	}))
	defer web.Close()
	socks := fakeSocks(t, map[string]string{"ip.test:80": web.Listener.Addr().String()})
	withIPTargets(t, []ipLookupTarget{{"ip.test", 80, false}})
	if got := fetchExitIP(socks); got != "" {
		t.Fatalf("hostile body produced %q", got)
	}
}

func TestFetchExitIPWhenProxyIsDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listening any more
	withIPTargets(t, []ipLookupTarget{{"ip.test", 80, false}})
	if got := fetchExitIP(addr); got != "" {
		t.Fatalf("got %q from a dead proxy", got)
	}
}

// The latency probes must still report a result when the IP lookup fails,
// and must attach the address when it works.
func TestRunProbeAttachesIPOnlyOnSuccess(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "203.0.113.99") }))
	defer web.Close()
	socks := fakeSocks(t, map[string]string{"ip.test:80": web.Listener.Addr().String()})
	withIPTargets(t, []ipLookupTarget{{"ip.test", 80, false}})

	r := runProbe("tcp", socks)
	if !r.OK || r.IP != "203.0.113.99" {
		t.Fatalf("tcp probe: %+v", r)
	}

	withIPTargets(t, []ipLookupTarget{{"nowhere.test", 80, false}})
	r = runProbe("tcp", socks)
	if !r.OK || r.IP != "" {
		t.Fatalf("latency should survive a failed lookup: %+v", r)
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	r = runProbe("tcp", dead)
	if r.OK || r.IP != "" || r.Error == "" {
		t.Fatalf("a failed probe must carry an error and no IP: %+v", r)
	}
}
