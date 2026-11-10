// muster-preview is the preview gateway: hosts running `muster-mirror
// -preview` open one tunnel to it each, and it will serve their local web
// servers to their owners (docs/design/previews.md, "The preview gateway").
// The logic is internal/previewgw; this file is its command line.
//
//	muster-preview [serve]                          run the gateway (the container's command)
//	muster-preview host-token create [<owner>] <host>
//	                                                mint a host token (printed once)
//	muster-preview host-token ls
//	muster-preview host-token revoke <id>
//	muster-preview version
//
// The host-token subcommands write $STATE_DIR/host-tokens.json under a flock;
// a running gateway re-reads it on every tunnel and every 30 s, so a revoked
// token's tunnel ends within 30 s. Run them where the gateway's STATE_DIR is
// mounted (inside its container).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ZacxDev/muster/internal/previewgw"
	"github.com/ZacxDev/muster/internal/telemetry"
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
	case "host-token":
		err = hostToken(args[1:], getenv, stdout)
	case "version", "--version":
		fmt.Fprintln(stdout, "muster-preview", previewgw.Version)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		err = fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
	if err != nil {
		fmt.Fprintln(stderr, "muster-preview:", err)
		return 1
	}
	return 0
}

const usage = `usage:
  muster-preview [serve]                       run the gateway
  muster-preview host-token create [<owner>] <host>
                                               mint a host token; <owner> is a login muster's
                                               GET /plugin/v1/self names for a token in
                                               PLUGIN_TOKENS_FILE (optional with one owner)
  muster-preview host-token ls
  muster-preview host-token revoke <id>
  muster-preview version
`

func serve(getenv func(string) string, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := previewgw.LoadConfig(getenv)
	if err != nil {
		log.Error("config", "err", err.Error())
		return 2
	}
	g, err := previewgw.New(cfg, previewgw.Options{Log: log})
	if err != nil {
		log.Error("start", "err", err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	// Tracing (internal/telemetry): on only when an OTLP endpoint is set, as
	// service muster-preview: the preview.request, preview.dial and
	// preview.ws spans (docs/design/previews.md, "Telemetry").
	flush, terr := telemetry.Setup(ctx, getenv, telemetry.Options{Service: "muster-preview", Version: previewgw.Version,
		Logger: slog.NewLogLogger(log.Handler(), slog.LevelInfo)})
	if terr != nil {
		log.Warn("tracing stays off", "err", terr.Error())
	}
	defer func() { _ = flush(context.Background()) }()
	if err := g.Run(ctx); err != nil {
		log.Error("run", "err", err.Error())
		return 1
	}
	return 0
}

func hostToken(args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("host-token needs create, ls or revoke\n" + usage)
	}
	dir, err := previewgw.CheckStateDir(getenv("STATE_DIR"))
	if err != nil {
		return err
	}
	store := previewgw.HostTokens{Dir: dir}
	switch args[0] {
	case "create":
		var login, host string
		switch len(args) {
		case 2:
			host = args[1]
		case 3:
			login, host = args[1], args[2]
		default:
			return errors.New("usage: host-token create [<owner>] <host>")
		}
		base, err := previewgw.BaseURL("MUSTER_URL", getenv("MUSTER_URL"))
		if err != nil {
			return err
		}
		toks, err := previewgw.ReadPluginTokens(getenv("PLUGIN_TOKENS_FILE"))
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		mc := previewgw.NewMusterClient(base, "muster-preview/"+previewgw.Version)
		owner, err := previewgw.ResolveOwner(login, mc.SelfAll(ctx, toks))
		if err != nil {
			return err
		}
		tok, rec, err := store.Create(owner.OwnerID, owner.OwnerLogin, host, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n# id %s, host %s, owner %s (id %d); shown once: store it as the host's preview token file\n",
			tok, rec.ID, rec.Host, rec.OwnerLogin, rec.OwnerID)
		return nil
	case "ls":
		if len(args) != 1 {
			return errors.New("usage: host-token ls")
		}
		toks, err := store.SortedForList()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tOWNER\tOWNER_ID\tHOST\tCREATED")
		for _, t := range toks {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", t.ID, t.OwnerLogin, t.OwnerID, t.Host, t.Created)
		}
		return tw.Flush()
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: host-token revoke <id>")
		}
		gone, err := store.Revoke(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "revoked %s (host %s); a running gateway ends its tunnel within %s\n",
			gone.ID, gone.Host, previewgw.RecheckEvery)
		return nil
	}
	return fmt.Errorf("unknown host-token command %q\n%s", args[0], usage)
}
