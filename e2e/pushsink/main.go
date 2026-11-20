// Command pushsink is the knock push spec's push service (e2e/tests/plugin-knock-push.spec.ts):
// a UnifiedPush-style endpoint a paired phone registers, served over TLS under
// a test host name with a certificate from a test CA it mints at start.
//
//	go build -o e2e/.bin/pushsink ./e2e/pushsink
//	e2e/.bin/pushsink -host push.example.test -proxy 127.0.0.1:<port> -ca-out <ca.pem> -record <received.jsonl>
//
// muster reaches it as it reaches any push host: an https endpoint on the
// default port (https://push.example.test/up/<topic>), on a host listed in
// MUSTER_PUSH_EXTRA_HOSTS, its certificate checked against the roots in
// SSL_CERT_FILE (which the spec points at -ca-out).
//
// ⚠ HOW :443 IS REACHED WITHOUT ROOT. Binding port 443 and naming a host in
// /etc/hosts both need root, which neither a developer's machine nor every CI
// runner gives a test. So the sink is ALSO an HTTP CONNECT proxy on -proxy,
// and the spec starts muster with HTTPS_PROXY pointing at it: Go's default
// transport, which muster's push client uses, then opens a tunnel to
// "push.example.test:443" through the proxy, and the TLS handshake (SNI, the
// chain against SSL_CERT_FILE) runs end to end inside it, exactly as it would
// to a listener on 443. The proxy accepts ONE target, exactly host + ":443",
// and refuses (and records) any other, so a send to another host or port is
// seen, not silently delivered. The tunnel is the only thing the proxy does:
// it never reads or answers inside it.
//
// Every POST over the tunnel is answered 201 and recorded as one JSON line in
// -record: {"path", "content_encoding", "ttl", "authorization", "body"
// (base64), "connect" (the tunnel's target)}. A refused CONNECT is recorded as
// {"refused": "<target>"}. The spec decrypts the bodies itself: it holds the
// device's key pair, which the sink never sees.
//
// It prints "pushsink: ready" on stdout once both listeners are up.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pushsink:", err)
		os.Exit(1)
	}
}

func run() error {
	host := flag.String("host", "push.example.test", "the push host name the certificate is for, and the only CONNECT target (with :443)")
	proxyAddr := flag.String("proxy", "", "the CONNECT proxy's listen address (host:port)")
	caOut := flag.String("ca-out", "", "where to write the test CA's certificate (PEM), for SSL_CERT_FILE")
	record := flag.String("record", "", "where to append one JSON line per request received")
	flag.Parse()
	if *proxyAddr == "" || *caOut == "" || *record == "" {
		return fmt.Errorf("-proxy, -ca-out and -record are required")
	}
	cert, caPEM, err := mint(*host)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*caOut, caPEM, 0o644); err != nil {
		return err
	}
	rec, err := newRecorder(*record)
	if err != nil {
		return err
	}

	// The TLS listener: loopback, an ephemeral port only the proxy dials.
	tlsLn, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	target := net.JoinHostPort(*host, "443")
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.write(map[string]any{
			"path": r.URL.Path, "content_encoding": r.Header.Get("Content-Encoding"), "ttl": r.Header.Get("TTL"),
			"authorization": r.Header.Get("Authorization") != "", "body": base64.StdEncoding.EncodeToString(body),
			"connect": target, "sni": r.TLS.ServerName,
		})
		w.WriteHeader(http.StatusCreated)
	})}
	go func() { _ = srv.Serve(tlsLn) }()

	proxyLn, err := net.Listen("tcp", *proxyAddr)
	if err != nil {
		return err
	}
	go serveProxy(proxyLn, target, tlsLn.Addr().String(), rec)

	fmt.Println("pushsink: ready")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	_ = proxyLn.Close()
	return srv.Close()
}

// serveProxy answers HTTP CONNECT for exactly target, tunnelling it to the
// TLS listener at backend; anything else is refused and recorded.
func serveProxy(ln net.Listener, target, backend string, rec *recorder) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			br := bufio.NewReader(c)
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			if req.Method != http.MethodConnect || req.Host != target {
				rec.write(map[string]any{"refused": req.Method + " " + req.Host})
				_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
				return
			}
			up, err := net.Dial("tcp", backend)
			if err != nil {
				_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
				return
			}
			defer up.Close()
			_ = c.SetReadDeadline(time.Time{})
			if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
				return
			}
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
			<-done
		}(c)
	}
}

// mint makes a throwaway CA and a leaf certificate for host signed by it.
func mint(host string) (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "muster e2e push sink test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, caPEM, nil
}

// recorder appends JSON lines to a file, one write per line.
type recorder struct {
	mu sync.Mutex
	f  *os.File
}

func newRecorder(path string) (*recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &recorder{f: f}, nil
}

func (r *recorder) write(v map[string]any) {
	b, _ := json.Marshal(v)
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.f.Write(append(b, '\n'))
	_ = r.f.Sync()
}
