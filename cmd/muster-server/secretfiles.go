package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ZacxDev/muster/internal/push"
)

// --- secrets from files (the _FILE twins) -----------------------------------
//
// A secret muster reads from the environment can instead be read from a file:
// NAME_FILE names it, and its contents (one trailing newline dropped) are the
// value. The plain variable WINS whenever it is set, so an install that sets
// its secrets in .env keeps exactly what it had.
//
// 🔴 A MISSING FILE IS AN UNSET VALUE, NOT AN ERROR. The Compose stack names a
// file for every secret, and muster-init writes only what the stack needs: an
// install whose .env sets the UI password gets no ui_password file, and one
// that never had push keys gets none (compose-init, below). Reading that as
// "unset" is exactly what the variable alone would mean. A file that EXISTS
// and cannot be read is a boot refusal: that is a broken mount, not a choice.

// withSecretFiles is getenv with every _FILE twin resolved: a variable that is
// empty in getenv and whose _FILE names a readable file reads as that file's
// contents. Every other variable reads exactly as getenv has it.
//
// The list of twins is the literal in withSecretFilesFrom, spelled where it is
// ranged over so internal/modulegate's env reader resolves each name it reads.
func withSecretFiles(getenv func(string) string) (func(string) string, error) {
	return withSecretFilesFrom(getenv, os.ReadFile)
}

// withSecretFilesFrom is withSecretFiles reading each _FILE through readFile:
// compose-init passes one that sees the files it is ABOUT to write, so it can
// check the configuration before it writes anything.
func withSecretFilesFrom(getenv func(string) string, readFile func(string) ([]byte, error)) (func(string) string, error) {
	resolved := map[string]string{}
	for _, s := range []struct{ env, file string }{
		{envDatabase, envDatabaseFile},
		{envUIPassword, envUIPasswordFile},
		{envHookToken, envHookTokenFile},
		{envVAPIDPublicKey, envVAPIDPublicKeyFile},
		{envVAPIDPrivateKey, envVAPIDPrivateKeyFile},
	} {
		if getenv(s.env) != "" {
			continue
		}
		path := strings.TrimSpace(getenv(s.file))
		if path == "" {
			continue
		}
		b, err := readFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s=%s: %v", s.file, path, err)
		}
		resolved[s.env] = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	// The wrapper reads nothing by a name of its own: it passes its caller's
	// name through, so it is not a read site (its callers' reads are), and it
	// calls getenv under another name to say so to the env reader.
	through := getenv
	return func(name string) string {
		if v, ok := resolved[name]; ok {
			return v
		}
		return through(name)
	}, nil
}

// --- compose-init: the Compose stack's one-shot ------------------------------
//
//	muster-server compose-init -dir /run/muster-secrets -database-url 'postgres://muster:{password}@postgres:5432/muster?sslmode=disable'
//
// Run by deploy/docker-compose.yml's muster-init service before Postgres and
// muster start, every time the stack comes up. It does two things:
//
//  1. SECRETS. Each one the environment (the stack's .env) does not set is
//     generated ONCE into -dir, a volume, and kept there on every later run:
//     the UI password (printed once, the run that makes it), the hook token,
//     and the Web Push keypair. Postgres's password is kept there too — the
//     .env's POSTGRES_PASSWORD the first time when it sets one, else generated
//     once — and muster's DATABASE_URL is written from it on every run.
//  2. THE CONFIGURATION CHECK. It reads muster's configuration exactly as the
//     server will (the same environment, the files as this run leaves them)
//     and runs the boot validation, plus the sign-in password's floor (the
//     server boots without it but refuses every browser page). A refusal is
//     printed and the one-shot exits 1, so `docker compose up --wait` fails in
//     seconds, naming muster-init, instead of muster restart-looping (or
//     sitting unhealthy) until a healthcheck gives up.
//
// 🔴 EVERY CHECK RUNS BEFORE ANY WRITE. The run first PLANS every file (what
// is kept, what is new), checks the configuration against that plan, and only
// then writes. A refused run changes nothing on the volume, so fixing the .env
// and running `up` again starts from exactly the state before.
//
// 🔴 POSTGRES'S PASSWORD FILE IS NEVER OVERWRITTEN FROM THE ENVIRONMENT.
// Postgres reads POSTGRES_PASSWORD_FILE only when it creates the database (an
// empty data volume); after that the password lives in the database. So once
// the file exists it is the database's password, and a POSTGRES_PASSWORD that
// differs from it is REFUSED, naming both ways out (remove the variable, or
// change the database's own password first: docs/quickstart-local.md). Writing
// it over the file would hand muster a DATABASE_URL the database rejects —
// measured on #707 round 0: a zero-config stack, then an .env from the example
// (POSTGRES_PASSWORD=CHANGE-ME), then that .env removed again, and muster could
// no longer log in to its own database. The variable is used only when there is
// no file yet: a new stack, or an .env install from before this command, whose
// database was created with exactly that variable.
//
// 🔴 AN .env THAT SETS POSTGRES_PASSWORD GETS NO GENERATED PUSH KEYS. Every
// .env written before this command existed sets it (the Compose file required
// it), and such an install's push is on or off by its own .env. Generating
// keys for it would turn push on behind the operator's back. A stack with no
// .env (or one that leaves POSTGRES_PASSWORD out) gets them, and push works
// out of the box.
//
// 🔴 NO PASSWORD IS GENERATED FOR A DATABASE THAT ALREADY EXISTS. With
// -postgres-data naming Postgres's data directory (mounted read-only), a run
// that finds no kept password but an initialised database (PG_VERSION) knows
// the database was created by an install from before this command, with the
// POSTGRES_PASSWORD its .env had then. That value is used when set; when it is
// not, the run is REFUSED, asking for it back. Generating one instead would
// keep a password the database never had, and the refusal a later run gives
// (the variable restored, differing from the kept file) would then advise
// keeping the wrong one — #707 round 2, F3.
//
// 🔴 ONE RUN AT A TIME. Two concurrent first runs on one volume (two `up`s at
// once) would each plan and print a sign-in password, and only the last
// rename would be kept. The run holds an exclusive flock on a lock file in
// -dir (`.lock`, empty, 0600) from the plan to the last write, so the second
// plans against the first's files. A regular file, not the directory: NFS and
// CIFS may refuse a lock on a directory. A filesystem that refuses locks at
// all (ENOLCK, EOPNOTSUPP) gets a WARNING and the run goes on unlocked — the
// race costs a printed password that may not be the kept one, and refusing
// to start the stack on such a volume would cost far more (#707 round 3).
//
// The files are mode 0400, owned by -owner (muster's uid:gid in its image);
// postgres_password by -postgres-owner (Postgres's).

const (
	secretPostgresPassword = "postgres_password"
	secretDatabaseURL      = "database_url"
	secretUIPassword       = "ui_password"
	secretHookToken        = "hook_token"
	secretVAPIDPublic      = "vapid_public_key"
	secretVAPIDPrivate     = "vapid_private_key"
)

// showSecretNames is what `muster-server show-secret` prints, by the name it
// takes: the two secrets a person needs to read back.
var showSecretNames = map[string]string{"ui-password": envUIPassword, "hook-token": envHookToken}

// showSecretHint is the command a person runs to read a secret back.
const showSecretHint = "docker compose exec muster /muster-server show-secret"

func runComposeInit(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("compose-init", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dir := fl.String("dir", "", "the directory (a volume) the secrets are kept in")
	dbTemplate := fl.String("database-url", "", "muster's DATABASE_URL, with {password} where Postgres's password goes")
	owner := fl.String("owner", "", "uid:gid that owns muster's files (empty: this process's)")
	pgOwner := fl.String("postgres-owner", "", "uid:gid that owns postgres_password (empty: this process's)")
	pgData := fl.String("postgres-data", "", "Postgres's data directory, read-only (empty: not checked): an initialised one never gets a generated password")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || !strings.Contains(*dbTemplate, "{password}") || fl.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: muster-server compose-init -dir <dir> -database-url 'postgres://user:{password}@host/db' [-owner uid:gid] [-postgres-owner uid:gid] [-postgres-data <dir>]")
		return 2
	}
	say := func(format string, a ...any) { fmt.Fprintf(stdout, "muster-init: "+format+"\n", a...) }
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "muster-init: "+format+"\n", a...)
		return 1
	}
	uid, gid, err := parseOwner(*owner)
	if err != nil {
		return fail("-owner: %v", err)
	}
	pgUID, pgGID, err := parseOwner(*pgOwner)
	if err != nil {
		return fail("-postgres-owner: %v", err)
	}
	st := secretStore{dir: *dir, uid: uid, gid: gid}
	pgStore := secretStore{dir: *dir, uid: pgUID, gid: pgGID}

	// One run at a time (see the header): held until this function returns.
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return fail("%v", err)
	}
	unlock, err := lockSecrets(*dir, func(format string, a ...any) {
		fmt.Fprintf(stderr, "muster-init: WARNING: "+format+"\n", a...)
	})
	if err != nil {
		return fail("lock %s: %v", *dir, err)
	}
	defer unlock()

	// THE PLAN: every file this run writes, and what each will hold. Nothing
	// is written until every check below has passed.
	type pending struct {
		store       secretStore
		name, value string
	}
	var writes []pending
	planned := map[string]string{} // cleaned path -> the file's contents after this run
	plan := func(s secretStore, name, value string) {
		writes = append(writes, pending{s, name, value})
		planned[filepath.Clean(s.path(name))] = value + "\n"
	}
	var made []string

	// 1. Postgres's password (kept once it exists), and muster's DATABASE_URL.
	pg, err := pgStore.read(secretPostgresPassword)
	if err != nil {
		return fail("%v", err)
	}
	switch envPG := getenv(envPostgresPassword); {
	case pg != "" && envPG != "" && envPG != pg:
		fmt.Fprintf(stderr, "muster-init: %s is set, but it is not the password this stack's database was created with "+
			"(kept in %s). Postgres reads its password only when it first creates the database, so with this value muster "+
			"could not log in to it. Nothing was changed. Either:\n"+
			"  - remove %s from .env: the stack keeps the password it has; or\n"+
			"  - change the database's own password first: with the variable removed, `up`, then ALTER USER through the "+
			"local socket (no password needed; psql's \\password asks for it and quotes it):\n"+
			"      docker compose exec postgres psql -U muster -d muster -c '\\password muster'\n"+
			"    and replace the kept copy: docs/quickstart-local.md#the-database-password\n"+
			"config: %s differs from the database's password; muster-init never overwrites it\n",
			envPostgresPassword, pgStore.path(secretPostgresPassword), envPostgresPassword, envPostgresPassword)
		return 1
	case pg != "":
		// Kept: the database's password.
	case envPG != "":
		pg = envPG
		plan(pgStore, secretPostgresPassword, pg)
	default:
		exists, err := databaseExists(*pgData)
		if err != nil {
			return fail("-postgres-data: %v", err)
		}
		if exists {
			fmt.Fprintf(stderr, "muster-init: this stack's database already exists, but its password was never kept here: "+
				"it was created by an install from before muster-init, with the %s its .env had then. "+
				"A generated one could not log in to it, so nothing was changed. "+
				"Put that line back in .env (the same value) and `up` again: it is kept from then on, "+
				"and the line can be removed after that start (docs/quickstart-local.md#the-database-password).\n"+
				"Lost it? Give the database a new one, in this order:\n"+
				"  1. put %s=<a new password> in .env\n"+
				"  2. docker compose up -d   (muster-init keeps the new value; muster cannot log in until step 3)\n"+
				"  3. docker compose exec postgres psql -U muster -d muster -c '\\password muster'\n"+
				"     and type that same new password twice (the local socket needs no password)\n"+
				"  4. docker compose up -d --wait   (muster logs in with it)\n"+
				"config: %s is not set, and the existing database needs the password it was created with\n",
				envPostgresPassword, envPostgresPassword, envPostgresPassword)
			return 1
		}
		pg = randomSecret(24)
		plan(pgStore, secretPostgresPassword, pg)
	}
	userinfo := strings.TrimPrefix(url.UserPassword("", pg).String(), ":")
	plan(st, secretDatabaseURL, strings.ReplaceAll(*dbTemplate, "{password}", userinfo))

	// 2. The UI password and the hook token: generated once unless .env sets them.
	var newUIPassword string
	if getenv(envUIPassword) == "" {
		kept, err := st.read(secretUIPassword)
		if err != nil {
			return fail("%v", err)
		}
		if kept == "" {
			newUIPassword = randomSecret(18)
			plan(st, secretUIPassword, newUIPassword)
			made = append(made, "the sign-in password")
		}
	}
	if getenv(envHookToken) == "" {
		kept, err := st.read(secretHookToken)
		if err != nil {
			return fail("%v", err)
		}
		if kept == "" {
			plan(st, secretHookToken, hex.EncodeToString(randomBytes(32)))
			made = append(made, "the hook token")
		}
	}

	// 3. Web Push keys: generated once, unless .env has its own (or is an .env
	// from before this command, which sets POSTGRES_PASSWORD; see the header).
	if getenv(envVAPIDPublicKey) == "" && getenv(envVAPIDPrivateKey) == "" && getenv(envPostgresPassword) == "" {
		pub, err := st.read(secretVAPIDPublic)
		if err != nil {
			return fail("%v", err)
		}
		priv, err := st.read(secretVAPIDPrivate)
		if err != nil {
			return fail("%v", err)
		}
		if pub == "" || priv == "" {
			priv, pub, err := push.GenerateVAPIDKeys()
			if err != nil {
				return fail("vapid: %v", err)
			}
			plan(st, secretVAPIDPublic, pub)
			plan(st, secretVAPIDPrivate, priv)
			made = append(made, "the Web Push keys")
		}
	}

	// 4. The configuration check, against the files as this run will leave
	// them: what the server would refuse at boot, and a sign-in password the
	// server would boot with but refuse every browser page behind.
	resolved, err := withSecretFilesFrom(getenv, func(path string) ([]byte, error) {
		if v, ok := planned[filepath.Clean(path)]; ok {
			return []byte(v), nil
		}
		return os.ReadFile(path)
	})
	if err != nil {
		return fail("config: %v", err)
	}
	cfg, err := loadConfig(resolved)
	if err == nil {
		err = cfg.validate()
	}
	if err == nil {
		if r := cfg.browserAuthRefusal(); r != "" {
			err = errors.New(r)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "muster-init: muster would refuse to start, so nothing was started or changed:\nconfig: %v\n", err)
		return 1
	}

	// 5. Every check passed: write the plan.
	for _, w := range writes {
		if err := w.store.write(w.name, w.value); err != nil {
			return fail("%v", err)
		}
	}
	if newUIPassword != "" {
		say("generated muster's sign-in password (shown this once):")
		say("%s     %s", secretLineMarker, newUIPassword)
		say("show it again any time: %s ui-password", showSecretHint)
	}
	if len(made) > 0 {
		say("generated %s into %s (kept on every later start)", strings.Join(made, ", "), *dir)
	}
	say("configuration OK")
	return 0
}

// secretLineMarker is on the one line muster-init prints a secret on (the
// generated sign-in password, for the person, once). 🔴 EVERY HELPER THAT
// SHOWS muster-init's LOGS TO AN AGENT DROPS THE LINE CARRYING IT:
// deploy/install.sh on a failed start and INSTALL.md's mi_logs (`grep -vF`),
// so the password never reaches an agent's transcript, which its provider
// reads (#728 round 0). Changing it changes those helpers' filter:
// TestComposeInitMarksTheOneLineThatHoldsThePassword and
// tests/install_guide_test.py read it from here.
const secretLineMarker = "[muster-secret]"

// databaseExists reports whether dir is an initialised Postgres data
// directory (it holds PG_VERSION). An empty dir argument is "not checked".
func databaseExists(dir string) (bool, error) {
	if dir == "" {
		return false, nil
	}
	_, err := os.Stat(filepath.Join(dir, "PG_VERSION"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// secretsLockName is the lock file compose-init holds in the secrets
// directory. It stays there, empty: the one file a refused run may leave.
const secretsLockName = ".lock"

// flock is syscall.Flock; a test swaps it to inject a filesystem's refusal.
var flock = syscall.Flock

// lockSecrets takes an exclusive flock on dir/.lock (created 0600 if it is
// missing), blocking until it is free, and returns its release. A filesystem
// that does not do locks (ENOLCK, EOPNOTSUPP: some NFS and CIFS mounts) is
// warned about and the run proceeds unlocked; any other error fails the run.
func lockSecrets(dir string, warn func(format string, a ...any)) (func(), error) {
	path := filepath.Join(dir, secretsLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		if errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.EOPNOTSUPP) {
			warn("cannot lock %s (%v): this filesystem does not support locks, so going on without one. "+
				"Two `up`s at once on this volume could each print a sign-in password; start one at a time", path, err)
			return func() { f.Close() }, nil
		}
		f.Close()
		return nil, err
	}
	return func() { _ = flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// runShowSecret prints one secret as muster resolves it (the environment, else
// its _FILE): `docker compose exec muster /muster-server show-secret ui-password`.
func runShowSecret(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 || showSecretNames[args[0]] == "" {
		fmt.Fprintln(stderr, "usage: muster-server show-secret ui-password|hook-token")
		return 2
	}
	resolved, err := withSecretFiles(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "show-secret: %v\n", err)
		return 1
	}
	v := resolved(showSecretNames[args[0]])
	if v == "" {
		fmt.Fprintf(stderr, "show-secret: %s is not set here (neither it nor its _FILE)\n", showSecretNames[args[0]])
		return 1
	}
	fmt.Fprintln(stdout, v)
	return 0
}

// secretStore writes secrets into dir, mode 0400, owned by uid:gid (-1: as is).
type secretStore struct {
	dir      string
	uid, gid int
}

func (s secretStore) path(name string) string { return filepath.Join(s.dir, name) }

// read returns name's kept value (one trailing newline dropped), or "" when
// there is none: no file, or a blank one (both mean "not made yet"). A file
// that exists and cannot be read is an error: a broken volume, not a choice.
func (s secretStore) read(name string) (string, error) {
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	v := strings.TrimSuffix(string(b), "\n")
	if strings.TrimSpace(v) == "" {
		return "", nil
	}
	return v, nil
}

// write replaces name with value, atomically (a temp file renamed over it).
func (s secretStore) write(name, value string) error {
	tmp, err := os.CreateTemp(s.dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return err
	}
	if s.uid >= 0 || s.gid >= 0 {
		if err := os.Chown(tmp.Name(), s.uid, s.gid); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), s.path(name))
}

// parseOwner reads "uid:gid"; empty is -1, -1 (leave the owner as is).
func parseOwner(s string) (int, int, error) {
	if s == "" {
		return -1, -1, nil
	}
	var uid, gid int
	if n, err := fmt.Sscanf(s, "%d:%d", &uid, &gid); err != nil || n != 2 || uid < 0 || gid < 0 {
		return 0, 0, fmt.Errorf("%q is not uid:gid", s)
	}
	return uid, gid, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on a supported platform
	}
	return b
}

// randomSecret is n random bytes, base64url without padding (4/3 n characters).
func randomSecret(n int) string { return base64.RawURLEncoding.EncodeToString(randomBytes(n)) }
