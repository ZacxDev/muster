package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/ZacxDev/muster/internal/telemetry"
)

type errSink struct {
	mu   sync.Mutex
	errs []string
}

func (s *errSink) Handle(err error) { s.mu.Lock(); s.errs = append(s.errs, err.Error()); s.mu.Unlock() }

// TestTheOTLPExporterRefusesARedirect: Setup's exporter, pointed at a
// collector that 307s to a second server, never sends that server a request
// — it would have been handed OTEL_EXPORTER_OTLP_HEADERS — and the export
// fails naming the refused host. The second server answers 200, so an
// exporter that followed the redirect would export there.
func TestTheOTLPExporterRefusesARedirect(t *testing.T) {
	var targetHits, fromHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	from := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromHits.Add(1)
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(from.Close)
	t.Setenv(telemetry.EnvTracesEndpoint, from.URL+"/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer test-key-not-real")
	t.Setenv(telemetry.EnvSampleRatio, "1")

	// Put back the process's provider, propagator and error handler.
	restore := telemetry.Use(otel.GetTracerProvider())
	t.Cleanup(restore)
	prevEH := otel.GetErrorHandler()
	sink := &errSink{}
	otel.SetErrorHandler(sink)
	t.Cleanup(func() { otel.SetErrorHandler(prevEH) })

	shutdown, err := telemetry.Setup(context.Background(), os.Getenv, telemetry.Options{Service: "muster-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "s")
	span.End()
	shutErr := shutdown(context.Background())

	if n := targetHits.Load(); n != 0 {
		t.Fatalf("the redirect target got %d requests, want 0", n)
	}
	if n := fromHits.Load(); n < 1 {
		t.Fatalf("the collector got %d requests, want at least 1 (the export never ran)", n)
	}
	sink.mu.Lock()
	all := strings.Join(append(sink.errs, errString(shutErr)), " | ")
	sink.mu.Unlock()
	if host := strings.TrimPrefix(target.URL, "http://"); !strings.Contains(all, "refused to follow a redirect") || !strings.Contains(all, host) {
		t.Fatalf("the export errors %q do not name the refused redirect to %s", all, host)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestTheExporterClientReadsTheTimeoutAndRefusesCertificates: handed a client,
// the exporter stops reading its timeout and certificate variables, so Setup
// does — a bad timeout or any certificate variable keeps tracing off.
func TestTheExporterClientReadsTheTimeoutAndRefusesCertificates(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		want string // "" = no error
	}{
		"a timeout":        {map[string]string{"OTEL_EXPORTER_OTLP_TIMEOUT": "2500"}, ""},
		"a bad timeout":    {map[string]string{"OTEL_EXPORTER_OTLP_TIMEOUT": "2s"}, "OTEL_EXPORTER_OTLP_TIMEOUT"},
		"a traces timeout": {map[string]string{"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT": "-1"}, "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT"},
		"a CA certificate": {map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": "/nonexistent/ca.pem"}, "OTEL_EXPORTER_OTLP_CERTIFICATE"},
		"a client key":     {map[string]string{"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY": "/nonexistent/key.pem"}, "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY"},
	} {
		env := map[string]string{telemetry.EnvTracesEndpoint: "http://127.0.0.1:1/v1/traces"}
		for k, v := range c.env {
			env[k] = v
		}
		restore := telemetry.Use(otel.GetTracerProvider())
		shutdown, err := telemetry.Setup(context.Background(), func(k string) string { return env[k] }, telemetry.Options{Service: "muster-test"})
		_ = shutdown(context.Background())
		restore()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err %v, want one naming %s", name, err, c.want)
		}
	}
}
