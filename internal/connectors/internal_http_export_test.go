package connectors

import (
	"net/http"
	"time"
)

// SetInternalHTTPTransportForTest replaces ih's transport, so a test can make
// the client fail in a shape a live service cannot produce on demand.
func SetInternalHTTPTransportForTest(ih *InternalHTTP, rt http.RoundTripper) {
	ih.client.Transport = rt
}

// SetInternalHTTPClientTimeoutForTest sets ih's http.Client.Timeout alone,
// leaving the call's context deadline at ih's configured timeout.
func SetInternalHTTPClientTimeoutForTest(ih *InternalHTTP, d time.Duration) { ih.client.Timeout = d }
