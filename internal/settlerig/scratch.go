// Package settlerig is the shared harness for running settlement against real
// processes: a throwaway database, the real worker and mock-provider binaries, and a
// verifier that checks the ledger against the provider's own books. The end-to-end
// tests (test/e2e) and the settlement benchmark (bench/settlement) both use it.
//
// Settlement is simulated: the "provider" is the mock payment server.
package settlerig

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scratchPrefix marks the databases this package may drop. Drop refuses any other name.
const scratchPrefix = "fifty_e2e_"

// Scratch is a throwaway database with every migration applied. Using one per run means
// a run starts from an empty jobs table and no pending settlements, whatever earlier
// tests and benchmarks left in the shared development database.
type Scratch struct {
	Name     string
	OwnerURL string // for migrations and administration
	AppURL   string // the unprivileged application role, as production runs
	Pool     *pgxpool.Pool

	adminURL string
}

// RepoRoot finds the module root by walking up from the working directory.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.Contains(string(b), "module github.com/riyne/fifty-fifty") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod for github.com/riyne/fifty-fifty above %s", dir)
		}
		dir = parent
	}
}

// withDatabase returns rawURL pointing at database name.
func withDatabase(rawURL, name string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// NewScratch creates a database, applies every migration with goose (run from the
// repository root, as `make migrate-up` does) and opens a pool as the application role.
// ownerURL must be a role that can create databases (the development owner).
func NewScratch(ctx context.Context, ownerURL, appURL string) (*Scratch, error) {
	root, err := RepoRoot()
	if err != nil {
		return nil, err
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	s := &Scratch{Name: scratchPrefix + hex.EncodeToString(b[:]), adminURL: ownerURL}
	if s.OwnerURL, err = withDatabase(ownerURL, s.Name); err != nil {
		return nil, err
	}
	if s.AppURL, err = withDatabase(appURL, s.Name); err != nil {
		return nil, err
	}

	admin, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return nil, fmt.Errorf("connect as owner: %w", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{s.Name}.Sanitize()); err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}

	cmd := exec.CommandContext(ctx, "go", "tool", "-modfile=tools/go.mod", "goose", "-dir", "migrations", "postgres", s.OwnerURL, "up")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = s.Drop(context.Background())
		return nil, fmt.Errorf("migrate: %v\n%s", err, out)
	}
	if s.Pool, err = pgxpool.New(ctx, s.AppURL); err != nil {
		_ = s.Drop(context.Background())
		return nil, fmt.Errorf("open pool: %w", err)
	}
	return s, nil
}

// Drop closes the pool and drops the database. It refuses a database this package did
// not create.
func (s *Scratch) Drop(ctx context.Context) error {
	if !strings.HasPrefix(s.Name, scratchPrefix) {
		return fmt.Errorf("refusing to drop %q: not a scratch database", s.Name)
	}
	if s.Pool != nil {
		s.Pool.Close()
	}
	admin, err := pgx.Connect(ctx, s.adminURL)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	_, err = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{s.Name}.Sanitize()+` WITH (FORCE)`)
	return err
}
