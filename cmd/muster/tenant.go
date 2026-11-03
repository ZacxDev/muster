package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// Tenants: one client, several muster servers (docs/design/multi-tenant.md,
// phase 1). A tenant is a separate muster deployment at its own URL with its
// own credential, and on this host it is ONE ENV FILE:
//
//	~/.muster/muster.env          tenant "default", exactly today's file
//	~/.muster/tenants/<name>.env  tenant <name> (0600, dir 0700)
//	~/.muster/current             one line: the current tenant; absent = default
//
// Selection, highest first: --tenant, MUSTER_TENANT, ~/.muster/current,
// "default". --env-file means "this file, not a tenant": it is refused next to
// --tenant and wins over MUSTER_TENANT and current.
//
// 🔴 THE ZERO-TOUCH PATH: no --tenant, no MUSTER_TENANT, no current file and
// NO ENTRY AT ALL under ~/.muster/tenants/ (a refused or malformed one counts).
// Only there is resolution today's, field by field (resolveConfig). Everywhere
// else the pair rule applies (resolvePaired): the host holds several servers'
// credentials, and a lone URL or token override would send one server's token
// to another.

const (
	// envTenant selects a tenant by name, below --tenant and above current.
	envTenant = "MUSTER_TENANT"
	// tenantLabelKey is an optional key INSIDE a tenant file: what a person
	// reads. It is never read from the environment.
	tenantLabelKey = "MUSTER_TENANT_LABEL"

	defaultTenant     = "default"
	tenantsDirPath    = "~/.muster/tenants"
	currentTenantPath = "~/.muster/current"
)

// tenantNameRE is the tenant-name grammar. The name is a file name, so this is
// also what keeps a name from walking out of the tenants directory.
var tenantNameRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// tenantFromEnv reads MUSTER_TENANT. It takes getenv as a parameter (rather
// than calling a.getenv inline) so internal/modulegate's env-var collector
// sees the read and holds docs/configuration.md to it.
func tenantFromEnv(getenv func(string) string) string { return getenv(envTenant) }

func validTenantName(name string) error {
	if !tenantNameRE.MatchString(name) {
		return failf(exitUsage, "invalid tenant name %q: want 1-32 of [a-z0-9-]", name)
	}
	return nil
}

// tenantSel is the outcome of tenant selection for one invocation.
type tenantSel struct {
	// zeroTouch: resolve exactly as before tenants existed.
	zeroTouch bool
	// name is the selected tenant; "" when --env-file chose the file.
	name string
	// source says where the choice came from: flag, env, file, default, or
	// env-file.
	source string
	// envFile is the file as displayed (with ~/); path is it expanded.
	envFile string
	path    string
	// inTenantsDir: the file is a tenant file, so its mode is checked.
	inTenantsDir bool
	// tenants is how many tenants this host has: default plus every
	// well-named tenant file.
	tenants int
}

// where names the selection in an error.
func (s tenantSel) where() string {
	if s.name == "" {
		return "--env-file " + s.envFile
	}
	return "tenant " + s.name + " (" + s.envFile + ")"
}

// tenantScan is what is on disk under ~/.muster.
type tenantScan struct {
	// hasEntries: ANY entry under tenants/ (or tenants/ not a readable dir).
	hasEntries bool
	// names: the well-named <name>.env entries, sorted.
	names []string
	// current: the current file's trimmed first line, and whether it exists.
	current       string
	currentExists bool
}

func (a *app) tenantsDir() string  { return expandHome(tenantsDirPath, a.homeDir) }
func (a *app) currentFile() string { return expandHome(currentTenantPath, a.homeDir) }

func (a *app) scanTenants() (tenantScan, error) {
	var s tenantScan
	dir := a.tenantsDir()
	fi, err := os.Lstat(dir)
	switch {
	case absent(err):
	case err != nil:
		// Cannot tell what is there: treat as tenants present (pair rule).
		s.hasEntries = true
	case !fi.IsDir():
		s.hasEntries = true
	default:
		ents, err := os.ReadDir(dir)
		if err != nil {
			s.hasEntries = true
			break
		}
		s.hasEntries = len(ents) > 0
		for _, e := range ents {
			n, ok := strings.CutSuffix(e.Name(), ".env")
			if ok && tenantNameRE.MatchString(n) && n != defaultTenant {
				s.names = append(s.names, n)
			}
		}
		sort.Strings(s.names)
	}

	cur := a.currentFile()
	if _, err := os.Lstat(cur); err == nil {
		s.currentExists = true
		b, err := os.ReadFile(cur)
		if err != nil {
			return s, wrapf(exitUsage, err, "reading %s", currentTenantPath)
		}
		line, _, _ := strings.Cut(string(b), "\n")
		s.current = strings.TrimSpace(line)
	} else if !absent(err) {
		return s, wrapf(exitUsage, err, "reading %s", currentTenantPath)
	}
	return s, nil
}

// absent: the path does not exist, or a parent of it is not a directory (a
// ~/.muster that is a file has no tenants in it). Either way there is nothing
// there, and a host in that state must stay on the zero-touch path.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// envFileChanged reports whether --env-file was given on the command line.
func (a *app) envFileChanged() bool {
	if a.envFileFlag != nil {
		return a.envFileFlag.Changed
	}
	return a.flagEnvFile != defaultEnvFile
}

// chooseTenant applies the selection precedence and returns the name and its
// source. It does not look for the tenant's file.
func (a *app) chooseTenant(s tenantScan) (name, source string, err error) {
	switch {
	case a.flagTenant != "":
		name, source = a.flagTenant, "flag"
	case tenantFromEnv(a.getenv) != "":
		name, source = tenantFromEnv(a.getenv), "env"
	case s.currentExists:
		if s.current == "" {
			return "", "", failf(exitUsage, "%s is empty: name a tenant in it, or run `tenant use --clear`", currentTenantPath)
		}
		name, source = s.current, "file"
	default:
		name, source = defaultTenant, "default"
	}
	if err := validTenantName(name); err != nil {
		return "", "", err
	}
	return name, source, nil
}

// selectTenant decides which file this invocation reads and which resolver
// applies to it.
func (a *app) selectTenant() (tenantSel, error) {
	envFileSet := a.envFileChanged()
	if a.flagTenant != "" && envFileSet {
		return tenantSel{}, failf(exitUsage,
			"--tenant and --env-file cannot be combined: --env-file means this file, not a tenant")
	}
	s, err := a.scanTenants()
	if err != nil {
		return tenantSel{}, err
	}
	sel := tenantSel{
		zeroTouch: a.flagTenant == "" && tenantFromEnv(a.getenv) == "" && !s.currentExists && !s.hasEntries,
		tenants:   1 + len(s.names),
	}
	if sel.zeroTouch || envFileSet {
		// The file the flag names (or its default), exactly as before.
		sel.envFile = a.flagEnvFile
		sel.path = expandHome(a.flagEnvFile, a.homeDir)
		if envFileSet {
			sel.source = "env-file"
			// A file under ~/.muster/tenants/ named by --env-file is still a
			// tenant file: it gets the same 0600 and not-a-symlink checks.
			sel.inTenantsDir = a.underTenantsDir(sel.path)
		} else {
			sel.name, sel.source = defaultTenant, "default"
		}
		return sel, nil
	}
	name, source, err := a.chooseTenant(s)
	if err != nil {
		return tenantSel{}, err
	}
	sel.name, sel.source = name, source
	if name == defaultTenant {
		sel.envFile = defaultEnvFile
		sel.path = expandHome(defaultEnvFile, a.homeDir)
		return sel, nil
	}
	sel.envFile = tenantsDirPath + "/" + name + ".env"
	sel.path = filepath.Join(a.tenantsDir(), name+".env")
	sel.inTenantsDir = true
	if _, err := os.Lstat(sel.path); errors.Is(err, fs.ErrNotExist) {
		return tenantSel{}, failf(exitUsage, "no tenant %q (selected by %s): %s does not exist; see `tenant list`",
			name, source, sel.envFile)
	}
	return sel, nil
}

// underTenantsDir reports whether path (as given, or with symlinks resolved)
// is inside ~/.muster/tenants/.
func (a *app) underTenantsDir(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	inside := func(dir, root string) bool {
		rel, err := filepath.Rel(root, dir)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
	}
	td := a.tenantsDir()
	if inside(filepath.Dir(abs), td) {
		return true
	}
	rd, err1 := filepath.EvalSymlinks(filepath.Dir(abs))
	rt, err2 := filepath.EvalSymlinks(td)
	return err1 == nil && err2 == nil && inside(rd, rt)
}

// checkTenantsDir refuses a tenants directory that is a symlink or not a
// directory. An absent one is fine (nothing is in it). display names it.
func checkTenantsDir(dir, display string) error {
	fi, err := os.Lstat(dir)
	switch {
	case absent(err):
		return nil
	case err != nil:
		return wrapf(exitUsage, err, "reading %s", display)
	case fi.Mode()&fs.ModeSymlink != 0:
		return failf(exitUsage, "%s is a symlink; it must be a real directory (mode 0700)", display)
	case !fi.IsDir():
		return failf(exitUsage, "%s is not a directory", display)
	}
	return nil
}

// readTenantFile opens a tenant file and refuses a symlink, a non-regular
// file, one readable by group or others, or one whose directory is a symlink.
// The file checks are on the OPENED descriptor (O_NOFOLLOW), so what is
// checked is what is read — the same shape as internal/mirrorhost's
// EnsureMCPSecret.
func readTenantFile(path, display string) (map[string]string, error) {
	if err := checkTenantsDir(filepath.Dir(path), filepath.Dir(display)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, failf(exitUsage, "%s is a symlink; a tenant file must be a regular file", display)
	}
	if err != nil {
		return nil, wrapf(exitUsage, err, "reading %s", display)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, wrapf(exitUsage, err, "reading %s", display)
	}
	if !fi.Mode().IsRegular() {
		return nil, failf(exitUsage, "%s is not a regular file", display)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, failf(exitUsage, "%s is readable by group or others (mode %04o); chmod 600 it", display, fi.Mode().Perm())
	}
	vals, err := parseEnv(f)
	if err != nil {
		return nil, wrapf(exitUsage, err, "reading %s", display)
	}
	return vals, nil
}

// loadSelected selects the tenant and reads its file.
func (a *app) loadSelected() (tenantSel, map[string]string, error) {
	sel, err := a.selectTenant()
	if err != nil {
		return tenantSel{}, nil, err
	}
	var vals map[string]string
	if sel.inTenantsDir {
		vals, err = readTenantFile(sel.path, sel.envFile)
	} else {
		vals, err = parseEnvFile(sel.path)
		if err != nil {
			err = wrapf(exitUsage, err, "reading %s", sel.path)
		}
	}
	if err != nil {
		return tenantSel{}, nil, err
	}
	return sel, vals, nil
}

// resolveSelected is the one place the resolver is chosen: today's on the
// zero-touch path, the pair rule everywhere else.
func resolveSelected(sel tenantSel, vals map[string]string, getenv func(string) string, flagURL, flagToken string) (config, error) {
	if sel.zeroTouch {
		return resolveConfig(sel.envFile, vals, getenv, flagURL, flagToken)
	}
	return resolvePaired(sel.where(), vals, getenv, flagURL, flagToken)
}

// tenantNote is the stderr label for a resolved selection, or "" when this
// host has one tenant (the label then adds nothing, and the zero-touch path
// has exactly one).
func tenantNote(sel tenantSel, apiURL string) string {
	if sel.tenants <= 1 {
		return ""
	}
	host := apiURL
	if u, err := url.Parse(apiURL); err == nil && u.Host != "" {
		host = u.Host
	}
	who := sel.name
	if who == "" {
		who = "--env-file " + sel.envFile
	}
	return "tenant: " + who + " (" + host + ")"
}

// originOf is scheme://host[:port] with the scheme's default port dropped, so
// http://h and http://h:80 are one origin.
func originOf(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", failf(exitUsage, "invalid API URL %q: want an absolute http(s) URL", raw)
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}

// ---------------------------------------------------------------------------
// `tenant …` — local verbs, except `add`, which calls /api/whoami once.

func newTenantCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Several muster servers on one host: list, add, use, remove, current",
		Long: `A tenant is a separate muster server with its own credential, kept in its own
env file: ~/.muster/tenants/<name>.env (0600). Today's ~/.muster/muster.env is
tenant "default". ~/.muster/current names the current tenant.

Selection, highest first: --tenant, ` + envTenant + `, ~/.muster/current, default.

With any tenant configured (a tenant file, a current file, ` + envTenant + ` or
--tenant), a URL override (` + envAPIURL + `, --api-url) and a token override
(` + envToken + `, ` + envTokenAlias + `, --token) are accepted only as a pair from
one source (both flags, or both environment variables): one without the other,
or a flag paired with a variable, is refused (exit 2) and nothing is sent.`,
	}
	cmd.AddCommand(newTenantListCmd(a), newTenantCurrentCmd(a), newTenantUseCmd(a), newTenantAddCmd(a), newTenantRemoveCmd(a))
	return cmd
}

type tenantRow struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Origin  string `json:"origin"`
	Current bool   `json:"current"`
	// Error is why this tenant's file cannot be used (its mode, say).
	Error string `json:"error,omitempty"`
}

// tenantRowFor reads one tenant's file for display. It never returns a token.
func (a *app) tenantRowFor(name string) tenantRow {
	r := tenantRow{Name: name, Label: name}
	var vals map[string]string
	var err error
	if name == defaultTenant {
		vals, err = parseEnvFile(expandHome(defaultEnvFile, a.homeDir))
	} else {
		vals, err = readTenantFile(filepath.Join(a.tenantsDir(), name+".env"), tenantsDirPath+"/"+name+".env")
	}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	if l := vals[tenantLabelKey]; l != "" {
		r.Label = l
	}
	if raw := vals[envAPIURL]; raw != "" {
		if o, err := originOf(raw); err == nil {
			r.Origin = o
		} else {
			r.Error = err.Error()
		}
	}
	return r
}

func (a *app) emitValue(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return wrapf(exitUsage, err, "encoding output")
	}
	return a.emit(raw)
}

func newTenantListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "One JSON row per tenant: name, label, origin, current (never a token)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := a.scanTenants()
			if err != nil {
				return err
			}
			current := ""
			if !a.envFileChanged() {
				if current, _, err = a.chooseTenant(s); err != nil {
					return err
				}
			}
			rows := []tenantRow{}
			for _, n := range append([]string{defaultTenant}, s.names...) {
				r := a.tenantRowFor(n)
				r.Current = n == current
				rows = append(rows, r)
			}
			return a.emitValue(rows)
		},
	}
}

func newTenantCurrentCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "The current tenant and where that choice came from (flag, env, file, default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sel, err := a.selectTenant()
			if err != nil {
				return err
			}
			out := struct {
				Name    string `json:"name"`
				Source  string `json:"source"`
				Label   string `json:"label"`
				Origin  string `json:"origin"`
				EnvFile string `json:"envFile"`
			}{Name: sel.name, Source: sel.source, EnvFile: sel.envFile}
			if sel.name != "" {
				r := a.tenantRowFor(sel.name)
				if r.Error != "" {
					return failf(exitUsage, "%s", r.Error)
				}
				out.Label, out.Origin = r.Label, r.Origin
			}
			return a.emitValue(out)
		},
	}
}

func newTenantUseCmd(a *app) *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "use <name> | --clear",
		Short: "Write ~/.muster/current (or delete it with --clear, falling back to default)",
		Long: `Make <name> the current tenant by writing ~/.muster/current atomically.

This re-targets every script on this host that runs the CLI without naming a
tenant; such a script should set ` + envTenant + `.

--clear deletes ~/.muster/current, so selection falls back to "default". The
host returns to resolving exactly as before tenants only once no tenant files
remain either (` + "`tenant remove`" + ` each one).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cur := a.currentFile()
			if clear {
				if len(args) != 0 {
					return failf(exitUsage, "tenant use: give a name or --clear, not both")
				}
				err := os.Remove(cur)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return wrapf(exitUsage, err, "removing %s", currentTenantPath)
				}
				return a.emitValue(map[string]any{"current": nil, "cleared": err == nil})
			}
			if len(args) != 1 {
				return failf(exitUsage, "tenant use: give a tenant name, or --clear")
			}
			name := args[0]
			if err := validTenantName(name); err != nil {
				return err
			}
			if name != defaultTenant {
				if _, err := os.Lstat(filepath.Join(a.tenantsDir(), name+".env")); err != nil {
					return failf(exitUsage, "no tenant %q: %s/%s.env does not exist; add it with `tenant add`",
						name, tenantsDirPath, name)
				}
			}
			if err := writeFileAtomic(cur, []byte(name+"\n")); err != nil {
				return wrapf(exitUsage, err, "writing %s", currentTenantPath)
			}
			return a.emitValue(map[string]any{"current": name})
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "delete ~/.muster/current; selection falls back to default")
	return cmd
}

// writeFileAtomic writes path through a temporary file in the same directory
// and a rename, mode 0600, creating the directory 0700 if needed.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".current-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func newTenantAddCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "add <name> --api-url <url>",
		Short: "Add a tenant: token from STDIN, checked with GET /api/whoami, saved 0600",
		Long: `Add tenant <name> for the muster server at --api-url. The token is read from
STDIN (first line), never from argv: argv is world-readable through /proc, so
--token is refused here.

Refused: the name "default", a name that already exists, and an origin another
tenant already has (two tenants on one origin would make "which tenant"
ambiguous). The token is checked with GET /api/whoami on that origin before
anything is written; the output names the origin and the principal pinned.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := validTenantName(name); err != nil {
				return err
			}
			if name == defaultTenant {
				return failf(exitUsage, "tenant add: %q is reserved for %s", defaultTenant, defaultEnvFile)
			}
			if a.flagToken != "" {
				return failf(exitUsage, "tenant add reads the token from STDIN, never from --token (argv is world-readable)")
			}
			if a.flagAPIURL == "" {
				return failf(exitUsage, "tenant add: --api-url is required")
			}
			origin, err := originOf(a.flagAPIURL)
			if err != nil {
				return err
			}
			dir := a.tenantsDir()
			if err := checkTenantsDir(dir, tenantsDirPath); err != nil {
				return err
			}
			path := filepath.Join(dir, name+".env")
			if _, err := os.Lstat(path); err == nil {
				return failf(exitUsage, "tenant add: tenant %q already exists (%s/%s.env)", name, tenantsDirPath, name)
			}
			s, err := a.scanTenants()
			if err != nil {
				return err
			}
			for _, n := range append([]string{defaultTenant}, s.names...) {
				if o := a.tenantOrigin(n); o == origin {
					return failf(exitUsage, "tenant add: origin %s is already tenant %q; two tenants on one origin would make \"which tenant\" ambiguous", origin, n)
				}
			}

			token, err := readTokenLine(cmd.InOrStdin())
			if err != nil {
				return err
			}
			cfg, err := checkAPIURL(config{apiURL: strings.TrimSpace(a.flagAPIURL), token: token})
			if err != nil {
				return err
			}
			c := newClient(cfg, a.newHTTP(a.flagTimeout), a.stderr)
			who, err := c.do(cmd.Context(), request{method: http.MethodGet, path: whoamiRoute, cred: credHook})
			if err != nil {
				return err
			}

			if err := prepareTenantsDir(dir); err != nil {
				return wrapf(exitUsage, err, "preparing %s", tenantsDirPath)
			}
			data := fmt.Sprintf("%s=%s\n%s=%s\n", envAPIURL, cfg.apiURL, envToken, token)
			if err := writeNewFileAtomic(dir, path, []byte(data)); err != nil {
				if errors.Is(err, fs.ErrExist) {
					return failf(exitUsage, "tenant add: tenant %q already exists (%s/%s.env)", name, tenantsDirPath, name)
				}
				return wrapf(exitUsage, err, "writing %s/%s.env", tenantsDirPath, name)
			}
			if strings.HasPrefix(origin, "http://") {
				fmt.Fprintf(a.stderr, "warning: tenant %q uses %s: its token travels in cleartext on every request; prefer https\n",
					name, origin)
			}
			return a.emitValue(map[string]any{"name": name, "origin": origin, "whoami": json.RawMessage(who)})
		},
	}
}

// prepareTenantsDir creates tenants/ 0700, refuses it as a symlink (re-checked
// here, just before the write), and tightens an existing one that group or
// others can reach to 0700.
func prepareTenantsDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is a symlink or not a directory", tenantsDirPath)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// writeNewFileAtomic writes data to a temporary file in dir (0600), fsyncs it,
// and LINKS it to path, so path appears whole or not at all. A link never
// replaces an existing name: if path exists (another `tenant add` won the
// race), it fails with fs.ErrExist and the existing file is untouched.
func writeNewFileAtomic(dir, path string, data []byte) error {
	f, err := os.CreateTemp(dir, ".add-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// tenantOrigin is a tenant's origin for the duplicate check, or "". A file
// whose mode is refused is still read for its URL: reading it sends nothing.
func (a *app) tenantOrigin(name string) string {
	path := expandHome(defaultEnvFile, a.homeDir)
	if name != defaultTenant {
		path = filepath.Join(a.tenantsDir(), name+".env")
	}
	vals, err := parseEnvFile(path)
	if err != nil || vals[envAPIURL] == "" {
		return ""
	}
	o, err := originOf(vals[envAPIURL])
	if err != nil {
		return ""
	}
	return o
}

// readTokenLine reads the token from stdin: the first line, trimmed.
func readTokenLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 64<<10)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", wrapf(exitUsage, err, "reading the token from stdin")
	}
	t := strings.TrimSpace(line)
	if t == "" {
		return "", failf(exitUsage, "tenant add: no token on stdin (pipe it in: the token is never taken from argv)")
	}
	return t, nil
}

func newTenantRemoveCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete a tenant's file; clears ~/.muster/current if it named that tenant",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := validTenantName(name); err != nil {
				return err
			}
			if name == defaultTenant {
				return failf(exitUsage, "tenant remove: %q is %s and is not removed by this command", defaultTenant, defaultEnvFile)
			}
			path := filepath.Join(a.tenantsDir(), name+".env")
			if err := os.Remove(path); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return failf(exitUsage, "no tenant %q: %s/%s.env does not exist", name, tenantsDirPath, name)
				}
				return wrapf(exitUsage, err, "removing %s/%s.env", tenantsDirPath, name)
			}
			s, err := a.scanTenants()
			if err != nil {
				return err
			}
			cleared := false
			if s.currentExists && s.current == name {
				if err := os.Remove(a.currentFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return wrapf(exitUsage, err, "removing %s", currentTenantPath)
				}
				cleared = true
			}
			return a.emitValue(map[string]any{"removed": name, "currentCleared": cleared})
		},
	}
}
