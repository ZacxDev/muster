package previewtunnel

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

// IdleCut is the negative control for criterion 4 on the real edge: with NO
// pings anywhere, how long after the last byte does each connection die?
// 0 means it was still alive when the measurement stopped.
type IdleCut struct {
	Max                          time.Duration
	TunnelCut, RelayCut, BareCut time.Duration
	TunnelErr, RelayErr, BareErr string
}

func (r IdleCut) String() string {
	f := func(d time.Duration) string {
		if d == 0 {
			return "alive at end"
		}
		return d.Round(100 * time.Millisecond).String()
	}
	return fmt.Sprintf("max=%v | host tunnel cut after %s (%s) | relayed WS (/ws) cut after %s (%s) | bare WS (/__bare_echo) cut after %s (%s)",
		r.Max, f(r.TunnelCut), r.TunnelErr, f(r.RelayCut), r.RelayErr, f(r.BareCut), r.BareErr)
}

// MeasureIdleCut opens a relayed and a bare WebSocket, echoes once on each, then
// sends nothing and records when each connection (and the host's tunnel) ends.
func MeasureIdleCut(e *Env, max time.Duration) (IdleCut, error) {
	r := IdleCut{Max: max}
	ctx, cancel := context.WithTimeout(context.Background(), max+10*time.Second)
	defer cancel()
	open := func(path string) (*websocket.Conn, error) {
		c, _, err := websocket.Dial(ctx, e.wsURL(path), e.dialOpts())
		if err != nil {
			return nil, err
		}
		if err := c.Write(ctx, websocket.MessageText, []byte("x")); err != nil {
			return nil, err
		}
		if _, p, err := c.Read(ctx); err != nil || string(p) != "x" {
			return nil, fmt.Errorf("first echo on %s: %v", path, err)
		}
		return c, nil
	}
	relay, err := open("/ws")
	if err != nil {
		return r, err
	}
	defer relay.CloseNow()
	bare, err := open("/__bare_echo")
	if err != nil {
		return r, err
	}
	defer bare.CloseNow()
	t0 := time.Now()
	type ev struct {
		which string
		at    time.Duration
		err   string
	}
	evs := make(chan ev, 3)
	watch := func(name string, c *websocket.Conn) {
		_, _, err := c.Read(ctx) // nothing is ever sent, so any return is the end
		evs <- ev{name, time.Since(t0), fmt.Sprint(err)}
	}
	go watch("relay", relay)
	go watch("bare", bare)
	go func() {
		select {
		case err := <-e.HostDone:
			evs <- ev{"tunnel", time.Since(t0), fmt.Sprint(err)}
		case <-ctx.Done():
		}
	}()
	stop := time.After(max)
	for got := 0; got < 3; {
		select {
		case x := <-evs:
			got++
			switch x.which {
			case "tunnel":
				r.TunnelCut, r.TunnelErr = x.at, x.err
			case "relay":
				r.RelayCut, r.RelayErr = x.at, x.err
			case "bare":
				r.BareCut, r.BareErr = x.at, x.err
			}
		case <-stop:
			return r, nil
		}
	}
	return r, nil
}
