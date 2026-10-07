package mai

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
)

// loopbackOnlyTransport permits requests only to loopback addresses and
// fails everything else, so tests cannot reach the real network by
// accident. Non-loopback tests must point at an httptest server, or be
// gated behind a MAI_LIVE_* switch.
type loopbackOnlyTransport struct {
	delegate http.RoundTripper
}

func (t loopbackOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		err := fmt.Errorf("test suite attempted a real network request to %q (%s); tests must not reach the network — point the endpoint at an httptest server, or gate the test behind a MAI_LIVE_* switch", host, req.URL)
		// Some clients wrap transport errors into generic messages, so also
		// shout on stderr where no error wrapping can swallow it.
		fmt.Fprintln(os.Stderr, "NETWORK GUARD:", err)
		return nil, err
	}
	return t.delegate.RoundTrip(req)
}

// liveProbesEnabled reports whether any MAI_LIVE_* environment switch is
// set, matching the convention that live probes require explicit opt-in.
func liveProbesEnabled() bool {
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "MAI_LIVE_") && value != "" {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	if !liveProbesEnabled() {
		http.DefaultTransport = loopbackOnlyTransport{delegate: http.DefaultTransport}
	}
	os.Exit(m.Run())
}
