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
// The list of twins is the literal below, spelled where it is ranged over so
// internal/modulegate's env reader resolves each name it reads.
func withSecretFiles(getenv func(string) string) (func(string) string, error) {
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
		b, err := os.ReadFile(path)
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
//     and the Web Push keypair. Postgres's password is written there too —
//     the .env's POSTGRES_PASSWORD when it sets one, else generated once — and
//     muster's DATABASE_URL is written from it on every run.
//  2. THE CONFIGURATION CHECK. It then reads muster's configuration exactly as
//     the server will (the same environment, the files just written) and runs
//     the boot validation. A refusal is printed and the one-shot exits 1, so
//     `docker compose up --wait` fails in seconds, naming muster-init, instead
//     of muster restart-looping until a healthcheck gives up.
//
// 🔴 AN .env THAT SETS POSTGRES_PASSWORD GETS NO GENERATED PUSH KEYS. Every
// .env written before this command existed sets it (the Compose file required
// it), and such an install's push is on or off by its own .env. Generating
// keys for it would turn push on behind the operator's back. A stack with no
// .env (or one that leaves POSTGRES_PASSWORD out) gets them, and push works
// out of the box.
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
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || !strings.Contains(*dbTemplate, "{password}") || fl.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: muster-server compose-init -dir <dir> -database-url 'postgres://user:{password}@host/db' [-owner uid:gid] [-postgres-owner uid:gid]")
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
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return fail("%v", err)
	}
	st := secretStore{dir: *dir, uid: uid, gid: gid}

	// 1. Postgres's password, and muster's DATABASE_URL from it.
	pg := getenv(envPostgresPassword)
	pgStore := secretStore{dir: *dir, uid: pgUID, gid: pgGID}
	switch {
	case pg != "":
		if err := pgStore.write(secretPostgresPassword, pg); err != nil {
			return fail("%v", err)
		}
	default:
		if pg, err = pgStore.keepOrMake(secretPostgresPassword, randomSecret(24)); err != nil {
			return fail("%v", err)
		}
	}
	userinfo := strings.TrimPrefix(url.UserPassword("", pg).String(), ":")
	if err := st.write(secretDatabaseURL, strings.ReplaceAll(*dbTemplate, "{password}", userinfo)); err != nil {
		return fail("%v", err)
	}

	// 2. The UI password and the hook token: generated once unless .env sets them.
	var made []string
	if getenv(envUIPassword) == "" {
		existed := st.exists(secretUIPassword)
		pw, err := st.keepOrMake(secretUIPassword, randomSecret(18))
		if err != nil {
			return fail("%v", err)
		}
		if !existed {
			made = append(made, "the sign-in password")
			say("generated muster's sign-in password (shown this once):")
			say("    %s", pw)
			say("show it again any time: %s ui-password", showSecretHint)
		}
	}
	if getenv(envHookToken) == "" {
		existed := st.exists(secretHookToken)
		if _, err := st.keepOrMake(secretHookToken, hex.EncodeToString(randomBytes(32))); err != nil {
			return fail("%v", err)
		}
		if !existed {
			made = append(made, "the hook token")
		}
	}

	// 3. Web Push keys: generated once, unless .env has its own (or is an .env
	// from before this command, which sets POSTGRES_PASSWORD; see the header).
	if getenv(envVAPIDPublicKey) == "" && getenv(envVAPIDPrivateKey) == "" && getenv(envPostgresPassword) == "" &&
		!(st.exists(secretVAPIDPublic) && st.exists(secretVAPIDPrivate)) {
		priv, pub, err := push.GenerateVAPIDKeys()
		if err != nil {
			return fail("vapid: %v", err)
		}
		if err := st.write(secretVAPIDPublic, pub); err != nil {
			return fail("%v", err)
		}
		if err := st.write(secretVAPIDPrivate, priv); err != nil {
			return fail("%v", err)
		}
		made = append(made, "the Web Push keys")
	}
	if len(made) > 0 {
		say("generated %s into %s (kept on every later start)", strings.Join(made, ", "), *dir)
	}

	// 4. The configuration check: what the server would refuse at boot.
	resolved, err := withSecretFiles(getenv)
	if err != nil {
		return fail("config: %v", err)
	}
	cfg, err := loadConfig(resolved)
	if err == nil {
		err = cfg.validate()
	}
	if err != nil {
		fmt.Fprintf(stderr, "muster-init: muster would refuse to start, so nothing was started:\nconfig: %v\n", err)
		return 1
	}
	say("configuration OK")
	return 0
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

func (s secretStore) exists(name string) bool {
	b, err := os.ReadFile(s.path(name))
	return err == nil && strings.TrimSpace(string(b)) != ""
}

// keepOrMake returns name's kept value, or writes fresh and returns it.
func (s secretStore) keepOrMake(name, fresh string) (string, error) {
	if b, err := os.ReadFile(s.path(name)); err == nil {
		if v := strings.TrimSuffix(string(b), "\n"); strings.TrimSpace(v) != "" {
			return v, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return fresh, s.write(name, fresh)
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
