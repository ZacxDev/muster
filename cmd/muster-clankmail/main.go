// muster-clankmail is a muster plugin for ClankMail, the signed-messaging
// protocol for agents (github.com/ZacxDev/clankmail): it delivers verified
// direct messages from pinned peers to the owner's muster chief, and sends the
// replies the owner approves. The logic is internal/clankmailplugin; this file
// is its command line.
//
//	muster-clankmail                      run the plugin (the container's command)
//	muster-clankmail keygen --out FILE    write a new key (one line: base64url seed, 0600)
//	muster-clankmail identity claim --key PRINCIPAL --name OWNER
//	muster-clankmail identity grant --key PRINCIPAL [--chief-key CHIEF] [--name chief]
//	muster-clankmail peer verify [--accept-principal] OWNER/NAME
//	muster-clankmail requests [--bodies]
//	muster-clankmail show
//
// The admin subcommands (peer verify, requests, show) talk to the running
// plugin over $STATE_DIR/admin.sock; run them inside the plugin's container.
// None of them sends a message: the plugin only sends replies the owner
// approved in muster.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/muster/internal/clankmailplugin"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "serve" {
		return serve(getenv, stderr)
	}
	var err error
	switch args[0] {
	case "keygen":
		err = keygen(args[1:], stdout)
	case "identity":
		err = identity(args[1:], getenv, stdout)
	case "peer":
		err = peer(args[1:], getenv, stdout)
	case "requests":
		err = requests(args[1:], getenv, stdout)
	case "show":
		err = admin(getenv, stdout, http.MethodGet, "/show", nil)
	case "version", "--version":
		fmt.Fprintln(stdout, "muster-clankmail", clankmailplugin.Version)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		err = fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
	if err != nil {
		fmt.Fprintln(stderr, "muster-clankmail:", err)
		return 1
	}
	return 0
}

const usage = `usage:
  muster-clankmail                      run the plugin
  muster-clankmail keygen --out FILE
  muster-clankmail identity claim --key PRINCIPAL_KEY --name OWNER
  muster-clankmail identity grant --key PRINCIPAL_KEY [--chief-key CHIEF_KEY] [--name chief]
  muster-clankmail peer verify [--accept-principal] OWNER/NAME
  muster-clankmail requests [--bodies]
  muster-clankmail show
`

func serve(getenv func(string) string, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := clankmailplugin.LoadConfig(getenv)
	if err != nil {
		log.Error("config", "err", err.Error())
		return 2
	}
	p, err := clankmailplugin.New(cfg, clankmailplugin.Options{Log: log, ClaimWait: 25})
	if err != nil {
		log.Error("start", "err", err.Error())
		return 1
	}
	defer p.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := p.Run(ctx); err != nil {
		log.Error("run", "err", err.Error())
		return 1
	}
	return 0
}

func keygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "the key file to create (must not exist; written 0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("keygen needs --out FILE")
	}
	k, err := clankmailplugin.WriteSeedFile(*out)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, clankmailplugin.KeyIDOf(k))
	return nil
}

// hubFlags: the hub to talk to, from flags or HUB_URL / HUB_NAME.
func hubFlags(fs *flag.FlagSet, getenv func(string) string) (*string, *string) {
	return fs.String("hub-url", getenv("HUB_URL"), "the hub's base URL (default $HUB_URL)"),
		fs.String("hub-name", getenv("HUB_NAME"), "the hub's DNS name (default $HUB_NAME)")
}

func hubClient(url, name string, key ed25519.PrivateKey) (*clankmailplugin.HubClient, error) {
	base, err := clankmailplugin.BaseURL("--hub-url", url)
	if err != nil {
		return nil, err
	}
	n, err := clankmailplugin.HubName(name)
	if err != nil {
		return nil, err
	}
	return &clankmailplugin.HubClient{Base: base, Name: n, Key: key, HTTP: &http.Client{Timeout: 30 * time.Second},
		Now: time.Now, UA: "muster-clankmail/" + clankmailplugin.Version}, nil
}

// identity: the owner's ceremony, run where the principal key is (it is meant
// to stay offline). claim binds @OWNER to the principal; grant names the chief
// key "chief" under it and enrolls the chief key (SPEC §6.1, §6.2).
func identity(args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) == 0 || (args[0] != "claim" && args[0] != "grant") {
		return errors.New("identity claim | identity grant")
	}
	fs := flag.NewFlagSet("identity "+args[0], flag.ContinueOnError)
	hubURL, hubName := hubFlags(fs, getenv)
	keyFile := fs.String("key", "", "the PRINCIPAL key file")
	name := fs.String("name", "", "claim: the handle to claim (OWNER); grant: the grant name (default chief)")
	chiefFile := fs.String("chief-key", getenv("CHIEF_KEY_FILE"), "grant: the chief key file (default $CHIEF_KEY_FILE)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *keyFile == "" {
		return errors.New("--key PRINCIPAL_KEY is required")
	}
	principal, err := clankmailplugin.ReadSeedFile(*keyFile)
	if err != nil {
		return fmt.Errorf("--key: %w", err)
	}
	hc, err := hubClient(*hubURL, *hubName, principal)
	if err != nil {
		return err
	}
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if args[0] == "claim" {
		if *name == "" {
			return errors.New("identity claim needs --name OWNER")
		}
		tok, _, err := clankmailplugin.SignObject(principal, "handle", now, map[string]any{"name": *name, "hub": hc.Name})
		if err != nil {
			return err
		}
		id, err := hc.Submit(ctx, tok)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "claimed @%s on %s for principal %s (handle %s)\n", *name, hc.Name, clankmailplugin.KeyIDOf(principal), id)
		return nil
	}
	gname := *name
	if gname == "" {
		gname = "chief"
	}
	if *chiefFile == "" {
		return errors.New("identity grant needs --chief-key CHIEF_KEY (or CHIEF_KEY_FILE)")
	}
	chief, err := clankmailplugin.ReadSeedFile(*chiefFile)
	if err != nil {
		return fmt.Errorf("--chief-key: %w", err)
	}
	gtok, gid, err := clankmailplugin.SignObject(principal, "grant", now,
		map[string]any{"agent": clankmailplugin.KeyIDOf(chief), "name": gname, "hub": hc.Name})
	if err != nil {
		return err
	}
	if _, err := hc.Submit(ctx, gtok); err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	etok, _, err := clankmailplugin.SignObject(chief, "enroll", time.Now().UnixMilli(), map[string]any{"grant": gid})
	if err != nil {
		return err
	}
	eid, err := hc.Submit(ctx, etok)
	if err != nil {
		return fmt.Errorf("enroll (the grant %s is live; enroll under it again): %w", gid, err)
	}
	fmt.Fprintf(stdout, "granted %q to %s (grant %s), enrolled (%s)\n", gname, clankmailplugin.KeyIDOf(chief), gid, eid)
	return nil
}

func stateDir(getenv func(string) string) (string, error) {
	d := getenv("STATE_DIR")
	if d == "" {
		return "", errors.New("STATE_DIR is required (the running plugin's state directory)")
	}
	return d, nil
}

func admin(getenv func(string) string, stdout io.Writer, method, path string, body any) error {
	d, err := stateDir(getenv)
	if err != nil {
		return err
	}
	b, err := clankmailplugin.AdminCall(context.Background(), d, method, path, body)
	if err != nil {
		return err
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		_, err = stdout.Write(b)
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func peer(args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "verify" {
		return errors.New("peer verify [--accept-principal] OWNER/NAME")
	}
	fs := flag.NewFlagSet("peer verify", flag.ContinueOnError)
	accept := fs.Bool("accept-principal", false, "accept a principal key that changed since the first verification")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("peer verify takes one OWNER/NAME")
	}
	return admin(getenv, stdout, http.MethodPost, "/peer-verify",
		clankmailplugin.PeerVerifyRequest{Peer: fs.Arg(0), AcceptPrincipal: *accept})
}

// requests lists the requests queue as the plugin recorded it. --bodies
// fetches each message from the hub (as the chief key, a recipient) and prints
// it QUOTED (Go %q: every control and non-ASCII character escaped), to this
// terminal only.
func requests(args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("requests", flag.ContinueOnError)
	bodies := fs.Bool("bodies", false, "also fetch and print each message's text (quoted)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := stateDir(getenv)
	if err != nil {
		return err
	}
	b, err := clankmailplugin.AdminCall(context.Background(), d, http.MethodGet, "/requests", nil)
	if err != nil {
		return err
	}
	var list struct {
		Count int `json:"count"`
		Items []struct {
			ID      string `json:"id"`
			From    string `json:"from"`
			Peer    string `json:"peer"`
			TS      int64  `json:"ts"`
			BodyLen int    `json:"body_len"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%d message(s) in the requests queue (from senders nobody allowed; never delivered to muster)\n", list.Count)
	var hc *clankmailplugin.HubClient
	if *bodies {
		k, err := clankmailplugin.ReadSeedFile(getenv("CHIEF_KEY_FILE"))
		if err != nil {
			return fmt.Errorf("--bodies needs CHIEF_KEY_FILE: %w", err)
		}
		if hc, err = hubClient(getenv("HUB_URL"), getenv("HUB_NAME"), k); err != nil {
			return err
		}
	}
	for _, it := range list.Items {
		who := it.From
		if it.Peer != "" {
			who += " (configured peer " + it.Peer + ")"
		}
		fmt.Fprintf(stdout, "%s  from %s  at %s  %d bytes\n", it.ID, who, time.UnixMilli(it.TS).UTC().Format(time.RFC3339), it.BodyLen)
		if hc == nil {
			continue
		}
		tok, found, err := hc.Object(context.Background(), it.ID)
		switch {
		case err != nil:
			fmt.Fprintln(stdout, "    (could not fetch:", err.Error()+")")
			continue
		case !found:
			fmt.Fprintln(stdout, "    (the hub no longer serves it)")
			continue
		}
		obj, err := clankmail.Verify(tok)
		if err != nil || obj.ID != it.ID {
			fmt.Fprintln(stdout, "    (the hub served bytes that do not verify as this message: not shown)")
			continue
		}
		subject, _ := obj.Fields["subject"].(string)
		body, _ := obj.Fields["body"].(string)
		fmt.Fprintf(stdout, "    UNTRUSTED subject: %s\n    UNTRUSTED body: %s\n", strconv.Quote(subject), strconv.Quote(body))
	}
	return nil
}
