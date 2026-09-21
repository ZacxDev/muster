{
  description = "muster — an agent work queue and dispatcher: tasks, provisioning, and a fleet chief";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      # Darwin is listed because everything here is pure Go and has no reason
      # not to build there. The IMAGE is Linux-only, and is exposed only on
      # Linux rather than failing at evaluation with a `dockerTools` error.
      #
      # 🔴 `x86_64-darwin` IS DELIBERATELY ABSENT, AND ITS ABSENCE IS
      # LOAD-BEARING. The nixpkgs this flake locks DROPPED that platform, so
      # listing it makes `nix flake show` and `nix flake check --all-systems`
      # exit 1 for the WHOLE flake, not just for that attribute — i.e. the two
      # canonical "is this flake healthy" commands go red while every CI job,
      # which builds explicit `x86_64-linux` attributes, stays green and cannot
      # see it. Re-add it only against a nixpkgs that still has it.
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      linuxSystems = [ "x86_64-linux" "aarch64-linux" ];

      forSystems = list: f: nixpkgs.lib.genAttrs list (system: f nixpkgs.legacyPackages.${system});
      forAll = forSystems systems;

      # 🔴 THE VERSION IS DERIVED, NEVER A LITERAL. A hand-maintained version
      # string is a claim nobody re-checks: it keeps its old value across the
      # commit that should have moved it, so an artefact ships mislabelled while
      # looking right, and every later reading of "which code is this" is wrong.
      # A git revision cannot disagree with the tree it was built from.
      # `dirtyShortRev` appears when the build came from a modified working
      # tree, which is itself the fact a reader of the tag most wants.
      version = self.shortRev or self.dirtyShortRev or "unknown";

      # 🔴 AN ALLOWLIST, NOT AN EXCLUDE LIST. A working tree of this repo will
      # hold caches, editor droppings and — the day someone runs the suite
      # against a local dump — data. An exclude list ships whatever nobody
      # thought to name. Flake source is already git-filtered, so untracked
      # files are not copied; but that is a property of how the source was
      # FETCHED, not of this derivation, and it does not hold for
      # `nix build path:.`. Stating the filter here makes the property true of
      # the derivation itself.
      #
      # 🔴 `migrations/*.sql` IS IN THIS FILTER AND ITS ABSENCE WOULD BE A BUILD
      # FAILURE, WHICH IS THE GOOD DIRECTION AND STILL WORTH NAMING. The files
      # are `//go:embed`ed by `internal/db`, and `loadMigrations` REFUSES an
      # embed that matched nothing — so a filter carrying only `.go` files fails
      # the build's own check phase with a message about the embed pattern, not
      # about the filter. Same class as `go.sum` below.
      onlyGo = pkgs: pkgs.lib.cleanSourceWith {
        src = ./.;
        name = "muster-source";
        filter = path: type:
          let rel = pkgs.lib.removePrefix (toString ./. + "/") (toString path);
          in
          (type == "directory" && (
            rel == "cmd" || rel == "internal"
            || pkgs.lib.hasPrefix "cmd/" rel || pkgs.lib.hasPrefix "internal/" rel
          ))
          || (rel == "go.mod")
          # 🔴 `go.sum` IS NAMED EXPLICITLY. `buildGoModule` with a real
          # `vendorHash` needs the lock file to resolve the module graph, so a
          # filter carrying `go.mod` alone stops the fetch phase dead. Spelled
          # as its own row rather than a `go.*` glob, for the reason the `.go`
          # row below gives: a glob is an allowlist that says something wider
          # than it means.
          || (rel == "go.sum")
          || ((pkgs.lib.hasPrefix "cmd/" rel || pkgs.lib.hasPrefix "internal/" rel)
              && (pkgs.lib.hasSuffix ".go" rel || pkgs.lib.hasSuffix ".sql" rel));
      };

      # 🔴 THE GO TOOLCHAIN IS PINNED, NOT INHERITED. `pkgs.go` follows nixpkgs
      # and will drift ahead of what `go.mod` declares and what CI's
      # `go-version-file` resolves — so the default would build muster under a
      # compiler nothing in this repo had ever run the tests under, and a lock
      # bump would move it again silently. Move this, `go.mod`'s `go` directive
      # and the CI job's toolchain together or not at all.
      #
      # 🔴 IT IS `buildGo125Module`, NOT `buildGoModule` WITH THE COMPILER IN
      # `nativeBuildInputs`. That spelling is a NO-OP for the pin:
      # `buildGoModule` uses the `go` from its OWN scope, so the build would
      # fetch whatever nixpkgs' default is while the derivation advertised 1.25
      # in its inputs — a pin that reads as one and is not.
      #
      # ⚠ `go_1_25` TRACKS A SERIES, NOT A PATCH RELEASE. The property this pin
      # holds is the MINOR version, which is what a language-version skew would
      # break.
      buildGoPinned = pkgs: pkgs.buildGo125Module;

      # 🔴 ONE BINDING, READ BY EVERY GO DERIVATION, BECAUSE THERE IS ONE
      # MODULE. Several literals would be several chances for one to go stale,
      # and a stale vendor hash is a build failure whose message points at the
      # derivation rather than at the `go.mod` change that caused it.
      #
      # To move it: change the dependency, set this to `lib.fakeHash`, run
      # `nix build .#muster-migrate`, and copy the hash nix prints.
      #
      # ⚠ A VENDOR HASH IS NOT A DEPENDENCY GATE AND MUST NOT BE READ AS ONE. It
      # pins the BYTES of whatever the module graph resolves to; it says nothing
      # about which modules are in that graph, and adding one just changes the
      # hash.
      goVendorHash = "sha256-904l7LfgjynBGYgWNeM2ltHuquifgNQWfWzoXiXAoHM=";

      mkMigrate = pkgs: (buildGoPinned pkgs) {
        pname = "muster-migrate";
        inherit version;
        src = onlyGo pkgs;
        vendorHash = goVendorHash;

        subPackages = [ "cmd/muster-migrate" ];

        # 🔴 THE UNIT TESTS RUN IN THE BUILD, NOT ONLY IN `checks`. A consumer
        # that pins this flake builds the PACKAGE and never runs `nix flake
        # check`, so a broken migration loader would otherwise reach a machine —
        # and `checks` is also where a `--no-link` CI job is easiest to forget.
        doCheck = true;

        # 🔴 `go test ./...`, SPELLED OUT, BECAUSE THE DEFAULT CHECK PHASE
        # HONOURS `subPackages` AND THAT MAKES IT VACUOUS. The mechanism, which
        # is the part worth understanding rather than copying: `subPackages`
        # exists to narrow what gets INSTALLED, and `buildGoModule`'s default
        # `checkPhase` reuses it to narrow the TEST WALK too. With
        # `subPackages = [ "cmd/muster-migrate" ]` the walk covers exactly one
        # directory — and every test in this module lives under `internal/`.
        # Measured on the project this style was taken from, the build log read
        #
        #     Running phase: checkPhase
        #     ?  .../cmd/cairn-server  [no test files]
        #     Running phase: installPhase
        #
        # — a GREEN check phase that ran ZERO tests. That is a reassuring zero
        # arriving through a build option whose only documented job is something
        # else.
        #
        # ⚠ AND THE TELL DOES NOT SURVIVE. The `[no test files]` line above is
        # what made it visible. The day `cmd/muster-migrate` gains a test, the
        # same narrowing would run ONE package and print a plausible-looking
        # PASS, with nothing in the log to say twelve others never ran.
        #
        # `go vet` is here too: it is the cheapest check that reads the code
        # rather than running it.
        #
        # 🔴 WHAT THIS CHECK PHASE STRUCTURALLY CANNOT SEE, SO NOBODY READS A
        # GREEN NIX BUILD AS A GREEN SUITE: the sandbox has no network and no
        # Postgres, and `MUSTER_TEST_REQUIRE_DB` is unset here — so every
        # Postgres-backed test SKIPS. That is deliberate (a build that needed a
        # database would not be a build), but it means this phase covers the
        # pure-Go half only. The CI `test` job sets both variables and is the
        # only place the store logic actually executes.
        checkPhase = ''
          runHook preCheck
          go vet ./...
          go test ./...
          runHook postCheck
        '';

        meta = with pkgs.lib; {
          description = "Apply muster's schema migrations to a Postgres database";
          homepage = "https://github.com/ZacxDev/muster";
          license = licenses.mit;
          mainProgram = "muster-migrate";
          platforms = platforms.unix;
        };
      };

      # 🔴 THE IMAGE CARRIES BUSYBOX AND DECLARES `PATH`, AND NEITHER IS
      # DECORATION. `buildLayeredImage` with `contents` set to the binary alone
      # produces an image with NO `PATH` and no `sh` — so `kubectl exec … -- sh`
      # and every `docker run --entrypoint sh` lands on "executable file not
      # found in $PATH" against an image that otherwise starts and works. For a
      # one-shot migration tool that is exactly when you need a shell: the run
      # failed and you want to look at the database.
      #
      # 🔴 CA ROOTS TOO, AND THE FAILURE WITHOUT THEM IS A CONFUSING ONE. A
      # managed Postgres reached over TLS (`sslmode=require`) needs a trust
      # store, and this image ships no `/etc` of its own — so the failure is a
      # certificate-verification error on a DSN that is entirely correct.
      #
      # ⚠ `Cmd` NAMES THE BINARY BY ABSOLUTE STORE PATH. `/bin/muster-migrate`
      # would also resolve, and that is the reason not to use it: the entrypoint
      # would then depend on `contents` placing the package's `bin/` at the image
      # root AND on `PATH`, so a change to either turns a wiring mistake into a
      # container that does not start.
      mkMigrateImage = pkgs:
        let migrate = mkMigrate pkgs;
        in
        pkgs.dockerTools.buildLayeredImage {
          name = "muster-migrate";
          tag = version;
          contents = [ migrate pkgs.busybox pkgs.cacert ];

          config = {
            Cmd = [ "${pkgs.lib.getExe migrate}" ];
            Env = [
              "PATH=/bin"
              "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
            ];
            User = "65532:65532";
            WorkingDir = "/";
          };
        };
    in
    {
      packages = forAll (pkgs:
        {
          muster-migrate = mkMigrate pkgs;
          default = mkMigrate pkgs;
        }
        // nixpkgs.lib.optionalAttrs
          (builtins.elem pkgs.stdenv.hostPlatform.system linuxSystems)
          {
            migrate-image = mkMigrateImage pkgs;
          });

      # 🔴 `apps.default` AND `packages.default` MOVE TOGETHER OR NOT AT ALL.
      # `nix run github:…/muster` resolves `apps.default` FIRST and only falls
      # back to `packages.default`'s `mainProgram`, so flipping one alone gives
      # ONE name two binaries — which one you get depends on the command you ran.
      apps = forAll (pkgs: {
        default = {
          type = "app";
          program = "${nixpkgs.lib.getExe (mkMigrate pkgs)}";
        };
        muster-migrate = {
          type = "app";
          program = "${nixpkgs.lib.getExe (mkMigrate pkgs)}";
        };
      });

      checks = forAll (pkgs: {
        muster-migrate = mkMigrate pkgs;
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            # The same toolchain the package and the CI job use — see
            # `buildGoPinned` above for why it is pinned rather than inherited.
            pkgs.go_1_25
            pkgs.git
            pkgs.python3
            # For `make test-db` and the compose file.
            pkgs.docker-client
            # `psql` / `pg_dump`: the schema comparison in CONTRIBUTING.md needs
            # both, and a shell that tells you to run them should carry them.
            pkgs.postgresql_16
          ];
          # 🔴 EVERY GATE, SPELLED OUT VERBATIM, so there is no gap between what
          # the shell says and what CI runs.
          shellHook = ''
            echo "muster dev shell."
            echo
            echo "start the test database (prints the two exports you need):"
            echo "    make test-db"
            echo "the Go suite, with the database REQUIRED rather than skipped:"
            echo "    MUSTER_TEST_REQUIRE_DB=1 go test -race -cover ./..."
            echo "build and vet:"
            echo "    go build ./... && go vet ./..."
            echo "and the leak gate, which must pass before any push —"
            echo "the self-test runs FIRST, and a scanner that cannot go red is testing nothing:"
            echo "    python3 tests/leakscan.py --self-test && python3 tests/leakscan.py"
          '';
        };
      });
    };
}
