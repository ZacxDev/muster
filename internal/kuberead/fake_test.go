package kuberead_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/kuberead"
)

// ---------------------------------------------------------------------------
// A FAKE API SERVER, served over TLS with its OWN certificate authority and a
// certificate whose only name is kubernetes.default.svc — so a reader that
// does not trust exactly that CA, or does not verify that server name, cannot
// connect (the listener is 127.0.0.1, which the certificate does not name).
//
// It serves the TokenRequest route (POST …/serviceaccounts/{sa}/token), which
// only the minter token may call, and hands out a fresh token per call, each
// a JWT-shaped string carrying a distinct jti. Every other request must carry
// a token it minted (never the minter's): anything else is a 403, and is
// recorded, so a test can assert what reached the API at all.
// ---------------------------------------------------------------------------

const (
	fakeMinter = "fixture-minter-token-not-real-9d1f" // a fixture, nobody's token
	fakeSANS   = "officers"
	fakeSAName = "officer-anomaly-prod-a"
)

type mintCall struct {
	Auth, Path string
	Expiration int64
}

type getCall struct {
	Auth, Method, Path, Query string
}

type fakeAPI struct {
	t     *testing.T
	srv   *httptest.Server
	caPEM []byte

	mu     sync.Mutex
	n      int
	issued map[string]string // token -> jti
	mints  []mintCall
	gets   []getCall
	// serve answers every non-mint request a minted token makes.
	serve func(w http.ResponseWriter, r *http.Request)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	caPEM, leaf := testPKI(t, "kubernetes.default.svc")
	f := &fakeAPI{t: t, caPEM: caPEM, issued: map[string]string{}}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.handle))
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

// endpoint is host:port of the fake, as the sealed config names it.
func (f *fakeAPI) endpoint() string { return f.srv.Listener.Addr().String() }

func (f *fakeAPI) cluster(t *testing.T) kuberead.Cluster {
	t.Helper()
	c, err := kuberead.NewCluster("prod-a", f.endpoint(), f.caPEM, fakeMinter)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	return c
}

func (f *fakeAPI) sa() kuberead.ServiceAccount {
	return kuberead.ServiceAccount{Namespace: fakeSANS, Name: fakeSAName}
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/token") {
		var body struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Spec       struct {
				ExpirationSeconds int64 `json:"expirationSeconds"`
			} `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.mints = append(f.mints, mintCall{Auth: auth, Path: r.URL.Path, Expiration: body.Spec.ExpirationSeconds})
		f.mu.Unlock()
		if auth != "Bearer "+fakeMinter || body.Kind != "TokenRequest" {
			http.Error(w, `{"kind":"Status","message":"forbidden"}`, http.StatusForbidden)
			return
		}
		f.mu.Lock()
		f.n++
		jti := fmt.Sprintf("jti-%d", f.n)
		tok := fakeJWT(jti, f.n)
		f.issued[tok] = jti
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "TokenRequest", "status": map[string]any{
			"token": tok, "expirationTimestamp": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)}})
		return
	}
	f.mu.Lock()
	f.gets = append(f.gets, getCall{Auth: auth, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
	_, minted := f.issued[strings.TrimPrefix(auth, "Bearer ")]
	serve := f.serve
	f.mu.Unlock()
	if !minted {
		http.Error(w, `{"kind":"Status","message":"not a minted token"}`, http.StatusForbidden)
		return
	}
	if serve == nil {
		http.Error(w, `{"kind":"Status","message":"not found"}`, http.StatusNotFound)
		return
	}
	serve(w, r)
}

func (f *fakeAPI) calls() ([]mintCall, []getCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mintCall(nil), f.mints...), append([]getCall(nil), f.gets...)
}

// fakeJWT is a JWT-SHAPED token (three base64url segments) whose payload
// carries jti. It is not signed: the fake only has to recognise it.
func fakeJWT(jti string, n int) string {
	enc := base64.RawURLEncoding.EncodeToString
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "fixture"})
	body, _ := json.Marshal(map[string]any{"jti": jti, "sub": "system:serviceaccount:" + fakeSANS + ":" + fakeSAName, "n": n})
	return enc(head) + "." + enc(body) + "." + enc([]byte(fmt.Sprintf("fixture-signature-%d", n)))
}

// serveJSON makes the fake answer every read with v.
func (f *fakeAPI) serveJSON(v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serve = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

// testPKI makes a CA and a leaf certificate for dnsName signed by it. It
// returns the CA's PEM and the leaf as a tls.Certificate.
func testPKI(t *testing.T, dnsName string) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	leaf, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), leaf
}
