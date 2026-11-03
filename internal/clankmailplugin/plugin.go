package clankmailplugin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Version is this plugin's release, served on /health and sent as its
// User-Agent.
const Version = "0.1.0"

// Crash points: where a test makes the process "die" (Options.Crash). Each one
// is a moment between two durable effects, which is where at-most-once and
// at-least-once are decided.
const (
	CrashAfterMuster = "relay:after-muster-2xx"   // muster holds the message; the cursor is not written
	CrashAfterLedger = "reply:after-ledger-write" // the signed reply is durable; nothing submitted
	CrashAfterSubmit = "reply:after-submit"       // the hub holds the reply; no result posted
)

// errCrash is what a crash hook returns: the step stops at once, writing
// nothing more, as if the process had been killed there.
var errCrash = errors.New("crash injected")

// Options are the seams a test (or main) fills.
type Options struct {
	Now       func() time.Time
	HTTP      *http.Client // for the hub and muster; nil = a 40 s timeout client
	Log       *slog.Logger
	Sleep     func(context.Context, time.Duration) error
	ClaimWait int // seconds a claim long-polls (0..25); main uses 25
	// Crash, when set, is called at each Crash* point; a non-nil error stops the
	// step there with nothing more written.
	Crash func(point string) error
}

// Plugin is one running muster-clankmail.
type Plugin struct {
	cfg     Config
	hub     *HubClient
	muster  *MusterClient
	st      *store
	log     *slog.Logger
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	wait    int
	crash   func(string) error
	peers   map[string]PeerConf // by key
	byLabel map[string]PeerConf
	health  *health
	self    string // our k_

	addrMu   sync.Mutex
	httpAddr string // the /health listener's address, once Run listens

	// relayMu serialises the inbox and requests polls: both settle messages
	// and walk state.held, and a peer's mail keeps its order only if one of
	// them runs at a time.
	relayMu sync.Mutex
}

// healthAddr is the address /health is served on (after Run started).
func (p *Plugin) healthAddr() string {
	p.addrMu.Lock()
	defer p.addrMu.Unlock()
	return p.httpAddr
}

// New opens the state (taking the single-writer lock) and reconciles the
// configured peers with it: a newly configured key starts `unverified`; a key
// no longer configured is dropped from the peers, while its allow-list entry
// stays tracked in `allowed` until the next peer sync DELETEs it at the hub.
// A listed request whose signer is now a pinned key is held for delivery
// (held.go adoptPinnedRequests).
func New(cfg Config, o Options) (*Plugin, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 40 * time.Second}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	st, err := openStore(cfg.StateDir, o.Now)
	if err != nil {
		return nil, err
	}
	ua := "muster-clankmail/" + Version
	p := &Plugin{
		cfg: cfg, st: st, log: o.Log, now: o.Now, sleep: o.Sleep, wait: o.ClaimWait, crash: o.Crash,
		hub:     &HubClient{Base: cfg.HubURL, Name: cfg.HubName, Key: cfg.Key, HTTP: o.HTTP, Now: o.Now, UA: ua},
		muster:  &MusterClient{Base: cfg.MusterURL, Token: cfg.PluginToken, HTTP: o.HTTP, UA: ua},
		peers:   map[string]PeerConf{},
		byLabel: map[string]PeerConf{},
		health:  &health{},
		self:    KeyIDOf(cfg.Key),
	}
	for _, pc := range cfg.Peers {
		p.peers[pc.Key] = pc
		p.byLabel[pc.Label] = pc
	}
	err = st.update(func(s *State) error {
		for k := range s.Peers {
			if _, ok := p.peers[k]; !ok {
				delete(s.Peers, k)
			}
		}
		for k, pc := range p.peers {
			ps := s.Peers[k]
			if ps == nil {
				s.Peers[k] = &PeerState{Label: pc.Label, Status: peerUnverified}
			} else if ps.Label != pc.Label {
				// The same key under a new label is a new decision: verify again.
				s.Peers[k] = &PeerState{Label: pc.Label, Status: peerUnverified}
			}
		}
		adoptPinnedRequests(s, p.peers, o.Now().Unix())
		return nil
	})
	if err != nil {
		st.close()
		return nil, err
	}
	return p, nil
}

// Close releases the state lock.
func (p *Plugin) Close() error { return p.st.close() }

func (p *Plugin) hit(point string) error {
	if p.crash == nil {
		return nil
	}
	if err := p.crash(point); err != nil {
		return errCrash
	}
	return nil
}

// peerFor returns the configured peer for key and whether its chain verified.
func (p *Plugin) peerFor(key string) (PeerConf, string, bool) {
	pc, ok := p.peers[key]
	if !ok {
		return PeerConf{}, "", false
	}
	status := peerUnverified
	p.st.view(func(s *State) {
		if ps := s.Peers[key]; ps != nil {
			status = ps.Status
		}
	})
	return pc, status, true
}

// Run serves /health and the admin socket and runs the loops until ctx ends.
func (p *Plugin) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	admin, err := p.listenAdmin()
	if err != nil {
		return err
	}
	defer admin.Close()
	ln, err := net.Listen("tcp", p.cfg.ListenAddr)
	if err != nil {
		return err
	}
	p.addrMu.Lock()
	p.httpAddr = ln.Addr().String()
	p.addrMu.Unlock()
	hs := &http.Server{Handler: p.healthHandler(), ReadHeaderTimeout: 5 * time.Second}
	as := &http.Server{Handler: p.adminHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	go func() { _ = as.Serve(admin) }()
	defer func() {
		shut, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = hs.Shutdown(shut)
		_ = as.Shutdown(shut)
	}()

	p.log.Info("muster-clankmail starting", "version", Version, "key", p.self, "hub", p.cfg.HubName,
		"peers", len(p.peers), "poll_interval", p.cfg.PollInterval.String())

	// One peer-sync attempt BEFORE any poll. A peer is `unverified` until its
	// first check, and its messages read before that are only held (held.go),
	// so this is what lets the first poll after a boot deliver them. It must
	// not block the loops: it is bounded, and when it fails the polls start
	// anyway (holding that peer's mail) while the peer loop retries on backoff.
	sctx, scancel := context.WithTimeout(ctx, 2*time.Minute)
	firstSync := p.SyncPeers(sctx)
	scancel()
	p.health.setPeers(firstSync)
	if firstSync != nil {
		p.log.Warn("initial peer sync failed: a pinned peer's messages are held until it verifies", "err", firstSync.Error())
	}

	var wg sync.WaitGroup
	// loop runs step every `every`. primed: step already ran once, returning
	// prior, so the loop waits (or backs off) before its first run.
	loop := func(name string, every time.Duration, step func(context.Context) error, report func(error), primed bool, prior error) {
		defer wg.Done()
		fails := 0
		if primed {
			wait := every
			if prior != nil {
				fails = 1
				wait = backoff(fails, every, 5*time.Minute)
			}
			if p.sleep(ctx, wait) != nil {
				return
			}
		}
		for ctx.Err() == nil {
			err := step(ctx)
			report(err)
			wait := every
			if err != nil {
				fails++
				wait = backoff(fails, every, 5*time.Minute)
				p.log.Warn(name+" failed", "err", err.Error(), "retry_in", wait.String())
			} else {
				fails = 0
			}
			if wait > 0 && p.sleep(ctx, wait) != nil {
				return
			}
		}
	}
	wg.Add(4)
	go loop("peer sync", peerCheckInterval, p.SyncPeers, p.health.setPeers, true, firstSync)
	go loop("inbox poll", p.cfg.PollInterval, p.PollInbox, p.health.setPoll, false, nil)
	go loop("requests poll", p.cfg.PollInterval, p.PollRequests, p.health.setRequests, false, nil)
	go loop("claim", 0, func(ctx context.Context) error {
		// A pending entry the hub cannot take yet must not stop the claims:
		// each claim is also what makes muster retry delivering queued
		// messages to the chief (docs/plugins.md).
		rerr := p.ResumePending(ctx)
		_, err := p.ClaimOnce(ctx)
		if err == nil {
			err = rerr
		}
		return err
	}, p.health.setClaim, false, nil)
	wg.Wait()
	return nil
}

// peerCheckInterval is how often each peer's chain is checked again.
const peerCheckInterval = 10 * time.Minute

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func backoff(n int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = 2 * time.Second
	}
	d := base
	for i := 0; i < n && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// ---- /health ----

type health struct {
	mu                                       sync.Mutex
	lastPoll, lastClaim, lastPeers, lastReqs time.Time
	pollErr, claimErr, peersErr, requestsErr string
}

func set(mu *sync.Mutex, at *time.Time, msg *string, err error) {
	mu.Lock()
	defer mu.Unlock()
	if err == nil {
		*at, *msg = time.Now(), ""
	} else {
		*msg = err.Error()
	}
}

func (h *health) setPoll(err error)     { set(&h.mu, &h.lastPoll, &h.pollErr, err) }
func (h *health) setClaim(err error)    { set(&h.mu, &h.lastClaim, &h.claimErr, err) }
func (h *health) setPeers(err error)    { set(&h.mu, &h.lastPeers, &h.peersErr, err) }
func (h *health) setRequests(err error) { set(&h.mu, &h.lastReqs, &h.requestsErr, err) }

// Summary is what /health and `show` report: counts and statuses, never
// message text.
func (p *Plugin) Summary() map[string]any {
	out := map[string]any{"status": "ok", "version": Version, "key": p.self, "hub": p.cfg.HubName}
	outcomes := map[string]int{}
	peers := []map[string]any{}
	ledger := map[string]int{}
	var requests, rejected, allowed, misthreaded, held int
	heldBy := map[string]int{} // by pinned label; a key no longer pinned as "unpinned " + its short id
	p.st.view(func(s *State) {
		for _, m := range s.Messages {
			outcomes[m.Outcome]++
		}
		heldKey := map[string]int{}
		for _, h := range s.Held {
			heldKey[h.From]++
			if pc, ok := p.peers[h.From]; ok {
				heldBy[pc.Label]++
			} else {
				heldBy["unpinned "+short(h.From)]++
			}
		}
		for k, ps := range s.Peers {
			_, allow := s.Allowed[k]
			peers = append(peers, map[string]any{"label": ps.Label, "key": k, "status": ps.Status,
				"reason": ps.Reason, "allowed": allow, "principal": ps.Principal, "held": heldKey[k]})
		}
		for _, e := range s.Ledger {
			ledger[e.Kind+":"+e.State]++
			if e.Kind == "reply" && !e.Reported && e.State != ledPending {
				ledger["unreported"]++
			}
		}
		requests, rejected, allowed = len(s.Requests), s.Rejected, len(s.Allowed)
		misthreaded = s.HubThreadMismatches
		held = len(s.Held)
	})
	p.health.mu.Lock()
	out["last_poll_ok"], out["poll_error"] = p.health.lastPoll, p.health.pollErr
	out["last_claim_ok"], out["claim_error"] = p.health.lastClaim, p.health.claimErr
	out["last_peers_ok"], out["peers_error"] = p.health.lastPeers, p.health.peersErr
	out["last_requests_ok"], out["requests_error"] = p.health.lastReqs, p.health.requestsErr
	p.health.mu.Unlock()
	out["messages"], out["peers"], out["ledger"] = outcomes, peers, ledger
	out["requests"], out["rejected"], out["allowed_keys"] = requests, rejected, allowed
	// Delivered messages the hub mis-threaded (its unsigned thread named another
	// root than the signed re chain): a hub that is lying or broken.
	out["hub_thread_mismatches"] = misthreaded
	// A pinned peer's messages waiting for that peer to verify (held.go): the
	// total, and per peer label (also on each peers row), so an operator sees
	// whose mail a `peer verify` would release before running it.
	out["held"], out["held_by_peer"] = held, heldBy
	return out
}

func (p *Plugin) healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.Summary())
	})
}
