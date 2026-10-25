package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// AssetLinksPath is where Android looks for a site's Digital Asset Links.
const AssetLinksPath = "/.well-known/assetlinks.json"

// assetLinkRelations are the relations the statement grants the app: only
// get_login_creds, which Android's credential manager checks before it runs a
// passkey ceremony for an app against this site's RP id (the app's WebView
// runs WebAuthn as the app, so the assertion's origin is
// android:apk-key-hash:…, not this site).
//
// ⚠ NOT handle_all_urls, though Android's passkey documentation shows it in
// its example. That is the App Links relation: the app declares no
// auto-verified intent filter, so it would only pre-grant this site's links to
// a future version of the app nobody decided to make a link handler.
var assetLinkRelations = []string{
	"delegate_permission/common.get_login_creds",
}

// androidPackage is Android's application-id shape: two or more dot-separated
// segments, each a letter followed by letters, digits or underscores.
var androidPackage = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)

// BuildAssetLinks renders /.well-known/assetlinks.json for one Android app:
// its package name and the SHA-256 fingerprints of its signing certificates
// (comma-separated; 64 hex digits each, with or without colons, any case).
//
// Both empty is "not configured": nil, nil, and the route answers 404. One
// without the other is an error, as is a malformed package or fingerprint:
// Android ignores a statement it cannot match, so a typo would otherwise ship
// as a silent "the app has no passkeys".
//
// Fingerprints are written as Android prints them (`AA:BB:…`, upper case),
// deduplicated, in the order given.
func BuildAssetLinks(pkg, certs string) ([]byte, error) {
	pkg = strings.TrimSpace(pkg)
	certs = strings.TrimSpace(certs)
	if pkg == "" && certs == "" {
		return nil, nil
	}
	if pkg == "" || certs == "" {
		return nil, fmt.Errorf("the package name and the certificate fingerprints are a pair and only one is set")
	}
	if !androidPackage.MatchString(pkg) {
		return nil, fmt.Errorf("%q is not an Android package name", pkg)
	}
	var fps []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(certs, ",") {
		fp, err := normalizeCertFingerprint(raw)
		if err != nil {
			return nil, err
		}
		if !seen[fp] {
			seen[fp] = true
			fps = append(fps, fp)
		}
	}
	type target struct {
		Namespace    string   `json:"namespace"`
		PackageName  string   `json:"package_name"`
		Fingerprints []string `json:"sha256_cert_fingerprints"`
	}
	type statement struct {
		Relation []string `json:"relation"`
		Target   target   `json:"target"`
	}
	return json.Marshal([]statement{{
		Relation: assetLinkRelations,
		Target:   target{Namespace: "android_app", PackageName: pkg, Fingerprints: fps},
	}})
}

// normalizeCertFingerprint turns one SHA-256 fingerprint into `AA:BB:…`.
func normalizeCertFingerprint(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	b, err := hex.DecodeString(strings.ReplaceAll(s, ":", ""))
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("%q is not a SHA-256 certificate fingerprint (64 hex digits, colons optional)", s)
	}
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02X", c)
	}
	return strings.Join(parts, ":"), nil
}

// handleAssetLinks is GET /.well-known/assetlinks.json: the Digital Asset
// Links statement that lets the Android app use this site's passkeys
// (Extensions.AssetLinks). OPEN: Android's verifier fetches it with no
// session, and it must answer 200 application/json with no redirect. It holds
// nothing per-user. Not configured: 404.
func (s *Server) handleAssetLinks(w http.ResponseWriter, _ *http.Request) {
	if len(s.ext.AssetLinks) == 0 {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(s.ext.AssetLinks)
}
