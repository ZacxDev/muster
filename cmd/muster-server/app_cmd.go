package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// runAppCommand is `muster-server app install|rotate|uninstall|list|unlink`
// (docs/design/meetings.md, "Who installs, and where", fork 6): the
// server-installed apps for deployments that install from a pipeline, in the
// family of `muster-server user disable`. It needs DATABASE_URL and
// MUSTER_PLUGIN_ORIGINS from the server's environment.
//
//	muster-server app install --name <slug> --label <tab label> --icon <key> \
//	    --origin <one of MUSTER_PLUGIN_ORIGINS> --entry <https URL> [--tasks] \
//	    [--link-to <id>]
//	muster-server app rotate <id>
//	muster-server app uninstall <id>
//	muster-server app list
//	muster-server app unlink <id>
//
// --link-to and unlink are the LINKED SERVER APPS (docs/design/contacts.md,
// "The core change: linked server apps"): an app installed --link-to <id> is
// linked to that app in the install's own transaction, and the two know each
// person by one subject. They are here and nowhere else (no page form, no
// bearer API): linking changes what two apps may join, and this command
// already needs the database access only an operator has.
//
// install and rotate print the token ONCE on stdout, and nothing else there.
// Audited as the command line (users.Actor{Via: "cli"}, no user):
// installed_by is NULL and installed_via is "cli". No bearer API is added.
//
// ⚠ AN UNINSTALL FROM HERE CANNOT WITHDRAW THE KNOCKS STILL OUT: which knocks
// are out is a running server's memory. The row, its grants, its task ledger
// and its unspent codes go as from the page; a device may keep a knock whose
// tap now opens a 404. Uninstall from /admin/apps to withdraw them.
func runAppCommand(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: muster-server app install --name <slug> --label <tab label> --icon <key> --origin <pattern> --entry <url> [--tasks] [--link-to <id>]\n"+
			"       muster-server app rotate|uninstall|unlink <id>\n"+
			"       muster-server app list")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	if len(cfg.PluginOrigins) == 0 {
		fmt.Fprintf(stderr, "%s is not set: a server app needs an origin it allows\n", envPluginOrigins)
		return 1
	}
	pool, err := db.Connect(ctx, cfg.Database)
	if err != nil {
		fmt.Fprintf(stderr, "db connect: %v\n", err)
		return 1
	}
	defer pool.Close()
	store := plugins.NewPG(pool)
	cli := users.Actor{Via: "cli"}
	id := func() (int64, bool) {
		if len(args) != 2 {
			return 0, false
		}
		n, err := strconv.ParseInt(args[1], 10, 64)
		return n, err == nil && n > 0
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("app install", flag.ContinueOnError)
		fs.SetOutput(stderr)
		name := fs.String("name", "", "the app's slug, unique among live server apps")
		label := fs.String("label", "", "the tab label")
		icon := fs.String("icon", "", "the tab icon's key (video, mic, notes, …)")
		origin := fs.String("origin", "", "one of MUSTER_PLUGIN_ORIGINS")
		entry := fs.String("entry", "", "the https entry URL under that origin")
		tasks := fs.Bool("tasks", false, "the app may file tasks for the people it acts for")
		linkTo := fs.Int64("link-to", 0, "an installed server app's id: the new app is linked to it, and the two know each person by one subject")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *linkTo < 0 {
			return usage()
		}
		tok, p, err := store.InstallServerApp(ctx, plugins.ServerAppIn{Name: *name, AppLabel: *label, AppIcon: *icon, Origin: *origin,
			EntryURL: *entry, Origins: cfg.PluginOrigins, Tasks: *tasks, LinkTo: *linkTo}, cli)
		if err != nil {
			fmt.Fprintf(stderr, "install: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, tok)
		fmt.Fprintf(stderr, "installed app %d (%s). The token above is shown only this once.\n", p.ID, p.Name)
		if *linkTo != 0 {
			fmt.Fprintf(stderr, "Linked with app %d: the two know each person by one subject, the one app %d already has for them.\n", *linkTo, *linkTo)
		}
		return 0
	case "rotate":
		n, ok := id()
		if !ok {
			return usage()
		}
		tok, _, err := store.RotateServerApp(ctx, n, cli)
		if err != nil {
			fmt.Fprintf(stderr, "rotate %d: %v\n", n, appCmdErr(err))
			return 1
		}
		fmt.Fprintln(stdout, tok)
		fmt.Fprintf(stderr, "app %d has a new token, shown above only this once. The old one is refused now: every call "+
			"the app is in ends until it is restarted with the new token.\n", n)
		return 0
	case "uninstall":
		n, ok := id()
		if !ok {
			return usage()
		}
		if _, err := store.UninstallServerApp(ctx, n, cli); err != nil {
			fmt.Fprintf(stderr, "uninstall %d: %v\n", n, appCmdErr(err))
			return 1
		}
		fmt.Fprintf(stdout, "app %d uninstalled: its token, grants, task ledger and unspent codes are gone.\n", n)
		fmt.Fprintln(stderr, "This command cannot withdraw a running server's knock notifications still out; uninstall from /admin/apps to withdraw them.")
		return 0
	case "list":
		if len(args) != 1 {
			return usage()
		}
		apps, err := store.ServerApps(ctx, cfg.PluginOrigins, time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "list: %v\n", err)
			return 1
		}
		for _, a := range apps {
			label := ""
			if a.App.AppLabel != nil {
				label = *a.App.AppLabel
			}
			entry := ""
			if a.App.EntryURL != nil {
				entry = *a.App.EntryURL
			}
			fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\t%v\t%s\t%d live\t%s\n", a.App.ID, a.App.Name, label, entry, a.App.Capabilities, a.InstalledVia, a.LiveGrants,
				linkedColumn(a.LinkedWith))
		}
		return 0
	case "unlink":
		n, ok := id()
		if !ok {
			return usage()
		}
		partner, err := store.UnlinkServerApp(ctx, n, cli)
		if err != nil {
			fmt.Fprintf(stderr, "unlink %d: %v\n", n, appCmdErr(err))
			return 1
		}
		fmt.Fprintf(stdout, "unlinked app %d from app %d.\n", n, partner)
		fmt.Fprintln(stderr, "People already known to both keep the same subject under both; "+
			"a person who first opens either app from now on gets a different subject under each.")
		return 0
	}
	return usage()
}

// linkedColumn is `app list`'s "linked with" column: the other app of the
// pair, or "-".
func linkedColumn(l *plugins.LinkedApp) string {
	if l == nil {
		return "-"
	}
	return fmt.Sprintf("linked with %d", l.ID)
}

// appCmdErr says ErrNotFound as a person reads it.
func appCmdErr(err error) error {
	if errors.Is(err, plugins.ErrNotFound) {
		return errors.New("no such installed app")
	}
	return err
}
