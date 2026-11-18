// Command mktoken mints a personal API token of scope agent-create for one
// muster user and prints it: the credential e2e/lib/seed.ts presents to POST
// /api/agents, the machine route that replaced the web UI's dispatch form
// (docs/design/chief-dispatch.md, phase 2).
//
//	DATABASE_URL=…_e2e MUSTER_E2E_USER=<id, default 1> go run ./e2e/mktoken
//
// It mints through the real token store, as that user, so row-level security
// files the token under its owner exactly as /account/tokens would.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check).
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mktoken:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	uid := authz.OperatorUserID
	if v := os.Getenv("MUSTER_E2E_USER"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("MUSTER_E2E_USER %q is not a user id", v)
		}
		uid = id
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	ctx = authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
	tok, _, err := apitokens.NewPG(pool).Create(ctx, "e2e agent-create", 0, apitokens.ScopeAgentCreate)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}
