package opencodehost

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// Credentials is the pair token muster-mirror reads opencode with, and the
// file it lives in. Renew replaces it before it expires — a token can mint
// its own replacement (POST /api/pair is accepted with a token wherever the
// password is) — and writes the new one to the same file, 0600.
type Credentials struct {
	// Path is the token file; "" only for a test's fixed token
	// (EnvCredentials), which cannot be renewed.
	Path string

	mu sync.Mutex
	tf TokenFile
	// renewErr is why the last renewal failed; nil once one succeeds.
	renewErr error
	// readErr is what the last authenticated request said: the token was
	// refused, or the server could not be reached; nil once one is answered.
	readErr error
}

// LoadCredentials reads the token file at path (ReadTokenFile).
func LoadCredentials(path string) (*Credentials, error) {
	tf, err := ReadTokenFile(path)
	if err != nil {
		return nil, err
	}
	return &Credentials{Path: path, tf: tf}, nil
}

// EnvCredentials is a fixed token for the server at base (CheckLocal), never
// renewed. 🔴 FOR TESTS ONLY: the daemon
// takes its token from the token file alone, because a token it cannot renew
// stops working, silently, 30 days on.
func EnvCredentials(base, token string) (*Credentials, error) {
	if err := CheckLocal(base); err != nil {
		return nil, err
	}
	if _, ok := Expiry(token); !ok {
		return nil, errors.New("not an opencode pair token")
	}
	return &Credentials{tf: TokenFile{URL: base, Token: token}}, nil
}

// token is the current token. Unexported: outside this package a token is
// used only through Client (whose requests the ledger pins) and named only
// by Fingerprint.
func (c *Credentials) token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tf.Token
}

// Fingerprint names the current token for a person (the package's
// Fingerprint).
func (c *Credentials) Fingerprint() string { return Fingerprint(c.token()) }

// URL is the server the token was minted by (a loopback url, not a secret).
func (c *Credentials) URL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tf.URL
}

// Expires is when the current token stops being accepted.
func (c *Credentials) Expires() time.Time {
	t, _ := Expiry(c.token())
	return t
}

// Client is a client of the token's server that authenticates with the
// current token at each request, and reports what each authenticated read
// says about the token to Health (observe).
func (c *Credentials) Client() (*Client, error) { return c.client() }

func (c *Credentials) client() (*Client, error) {
	cl, err := NewClient(c.URL(), c.token)
	if err != nil {
		return nil, err
	}
	cl.report = c.observe
	return cl, nil
}

// observe records what an authenticated request said about the token: a
// refusal (ErrUnauthorized) or an unreachable server, or nil — answered —
// which clears either.
func (c *Credentials) observe(err error) {
	c.mu.Lock()
	c.readErr = err
	c.mu.Unlock()
}

// ErrNotRenewable: a fixed token (EnvCredentials) has no file to keep a new
// one in.
var ErrNotRenewable = errors.New("this opencode token has no token file, so it is not renewed: pair the host (muster-mirror opencode-pair)")

// Renew replaces the token when it expires within RenewBefore of now: it
// mints a new one with the current token, checks it outlives the old one,
// writes it to the token file (0600, whole) and only then uses it. renewed is
// false, with no error, when the token is not yet due.
//
// 🔴 THE FILE IS WRITTEN BEFORE THE NEW TOKEN IS USED: a muster-mirror that
// dies between the two restarts on the new token, never on one it has
// already replaced. Both tokens stay valid until their own expiry (tokens are
// stateless upstream), so the order costs nothing.
func (c *Credentials) Renew(ctx context.Context, now time.Time) (renewed bool, err error) {
	defer func() {
		c.mu.Lock()
		if renewed {
			c.renewErr = nil
		} else if err != nil {
			c.renewErr = err
		}
		c.mu.Unlock()
	}()
	old := c.token()
	exp, _ := Expiry(old)
	if exp.Sub(now) > RenewBefore {
		return false, nil
	}
	if c.Path == "" {
		return false, ErrNotRenewable
	}
	if !exp.After(now) {
		return false, fmt.Errorf("the opencode token (fingerprint %s) expired %s; pair the host again (muster-mirror opencode-pair)",
			Fingerprint(old), exp.UTC().Format(time.RFC3339))
	}
	cl, err := c.Client()
	if err != nil {
		return false, err
	}
	tok, err := cl.mint(ctx)
	if err != nil {
		return false, fmt.Errorf("renew the opencode token: %w", err)
	}
	if nexp, _ := Expiry(tok); !nexp.After(exp) {
		return false, errors.New("renew the opencode token: the new token does not outlive the old one")
	}
	tf := TokenFile{URL: c.URL(), Token: tok}
	if err := WriteTokenFile(c.Path, tf); err != nil {
		return false, fmt.Errorf("renew the opencode token: write %s: %w", c.Path, err)
	}
	c.mu.Lock()
	c.tf = tf
	c.mu.Unlock()
	return true, nil
}

// RenewEvery is how often KeepRenewed checks the token.
const RenewEvery = time.Hour

// KeepRenewed renews c (Renew) now and every RenewEvery until ctx ends,
// logging each renewal and each failure by fingerprint only. It returns at
// once for a token that cannot be renewed (logging so).
func KeepRenewed(ctx context.Context, c *Credentials, logger *log.Logger, every time.Duration, now func() time.Time) {
	if c.Path == "" {
		logger.Printf("muster-mirror: %v", ErrNotRenewable)
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		old := c.token()
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		renewed, err := c.Renew(rctx, now())
		cancel()
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Printf("muster-mirror: opencode token %s expires %s; %v (retrying in %s)",
				Fingerprint(old), c.Expires().UTC().Format(time.RFC3339), err, every)
		case renewed:
			logger.Printf("muster-mirror: renewed the opencode token: %s → %s, expires %s, written to %s (0600)",
				Fingerprint(old), Fingerprint(c.token()), c.Expires().UTC().Format(time.RFC3339), c.Path)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// WarnBefore is how close to its expiry a token is reported to the person
// (Health) even while no renewal has failed: renewal starts at RenewBefore,
// so a token this close has not been renewed for a week.
const WarnBefore = 3 * 24 * time.Hour

// Health is what the person must be told about the token at now, "" when
// nothing: that it EXPIRED; that it expires soon while renewal is FAILING (and
// why); or that it expires within WarnBefore. 🔴 THE ONE SPELLING: the
// rendered view of an opencode window shows it, and `-list -opencode` prints
// it, so a renewal that keeps failing is seen in muster, not only in a log.
func (c *Credentials) Health(now time.Time) string {
	_, line := c.health(now)
	return line
}

// HealthState is Health's STATE, without the countdown: "" for nothing,
// else "expired", "refused", "unreachable: <why>", "failing: <why>" or
// "soon". It changes only when what the person must do changes, so a view
// redraws on it rather than on every tick of "expires in …".
func (c *Credentials) HealthState(now time.Time) string {
	state, _ := c.health(now)
	return state
}

func (c *Credentials) health(now time.Time) (state, line string) {
	c.mu.Lock()
	renewErr, readErr := c.renewErr, c.readErr
	c.mu.Unlock()
	exp := c.Expires()
	left := exp.Sub(now)
	switch {
	case left <= 0:
		return "expired", "the opencode token expired " + exp.UTC().Format(time.RFC3339) + ": pair this host again with `muster-mirror opencode-pair`"
	case errors.Is(readErr, ErrUnauthorized):
		return "refused", "opencode refused the token: pair this host again with `muster-mirror opencode-pair`"
	case readErr != nil:
		return "unreachable: " + readErr.Error(), "the opencode server cannot be reached: " + readErr.Error()
	case renewErr != nil:
		return "failing: " + renewErr.Error(), "opencode token expires in " + approx(left) + " — renewal failing: " + renewErr.Error()
	case left <= WarnBefore:
		return "soon", "opencode token expires in " + approx(left) + " and has not been renewed"
	}
	return "", ""
}

// approx is d as a person reads it: whole days, else whole hours, else
// minutes.
func approx(d time.Duration) string {
	plural := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d >= 24*time.Hour:
		return plural(int64(d/(24*time.Hour)), "day")
	case d >= time.Hour:
		return plural(int64(d/time.Hour), "hour")
	}
	return plural(max(int64(d/time.Minute), 1), "minute")
}
