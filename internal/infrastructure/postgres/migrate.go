package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/config"
)

type MigrationRunner struct {
	pool  *pgxpool.Pool
	files fs.FS
}

func RegisterMigrationLifecycle(
	lifecycle fx.Lifecycle,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	cfg config.Config,
	files fs.FS,
) {
	runner := NewMigrationRunner(pool, files)
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			startupCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
			defer cancel()
			if err := runner.Up(startupCtx); err != nil {
				pool.Close()
				return fmt.Errorf("apply PostgreSQL migrations: %w", err)
			}
			logger.Info("PostgreSQL migrations are current")
			return nil
		},
	})
}

func NewMigrationRunner(pool *pgxpool.Pool, files fs.FS) *MigrationRunner {
	return &MigrationRunner{pool: pool, files: files}
}

func (m *MigrationRunner) Up(ctx context.Context) error {
	migrations, err := loadMigrations(m.files)
	if err != nil {
		return err
	}
	if err := m.ensureMigrationTable(ctx); err != nil {
		return err
	}

	for _, migration := range migrations {
		tx, err := m.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", migration.version, err)
		}
		upSQL, checksum, err := migrationSource(m.files, migration)
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("read migration %d: %w", migration.version, err)
		}
		_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x4a554e474c454d))
		if err == nil {
			var storedChecksum string
			err = tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version = $1`, migration.version).Scan(&storedChecksum)
			if errors.Is(err, pgx.ErrNoRows) {
				err = nil
				_, err = tx.Conn().PgConn().Exec(ctx, string(upSQL)).ReadAll()
				if err == nil {
					_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`, migration.version, checksum)
				}
			} else if err == nil && storedChecksum != checksum {
				err = fmt.Errorf("migration %d checksum changed: database has %s, source has %s", migration.version, storedChecksum, checksum)
			}
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %d: %w", migration.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %d: %w", migration.version, err)
		}
	}
	return nil
}

func (m *MigrationRunner) Down(ctx context.Context, count int) error {
	if count <= 0 {
		return fmt.Errorf("migration down count must be positive")
	}
	migrations, err := loadMigrations(m.files)
	if err != nil {
		return err
	}
	if err := m.ensureMigrationTable(ctx); err != nil {
		return err
	}

	for range count {
		tx, err := m.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin down migration: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x4a554e474c454d)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("lock migrations: %w", err)
		}
		var version int64
		var storedChecksum string
		err = tx.QueryRow(ctx, `SELECT version, checksum FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &storedChecksum)
		if err != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("find latest migration: %w", err)
		}
		migration, ok := findMigration(migrations, version)
		if !ok {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("no down migration found for version %d", version)
		}
		_, checksum, err := migrationSource(m.files, migration)
		if err == nil && checksum != storedChecksum {
			err = fmt.Errorf("migration %d checksum changed: database has %s, source has %s", migration.version, storedChecksum, checksum)
		}
		var downSQL []byte
		if err == nil {
			downSQL, err = fs.ReadFile(m.files, migration.down)
		}
		if err == nil {
			_, err = tx.Conn().PgConn().Exec(ctx, string(downSQL)).ReadAll()
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("revert migration %d: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit down migration %d: %w", version, err)
		}
	}
	return nil
}

func (m *MigrationRunner) ensureMigrationTable(ctx context.Context) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration table setup: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x4a554e474c454d)); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("lock migration table setup: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			checksum CHAR(64) NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("create schema migrations table: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration table setup: %w", err)
	}
	return nil
}

func migrationSource(files fs.FS, migration migration) ([]byte, string, error) {
	up, err := fs.ReadFile(files, migration.up)
	if err != nil {
		return nil, "", err
	}
	down, err := fs.ReadFile(files, migration.down)
	if err != nil {
		return nil, "", err
	}
	content := make([]byte, 0, len(up)+1+len(down))
	content = append(content, up...)
	content = append(content, 0)
	content = append(content, down...)
	digest := sha256.Sum256(content)
	return up, hex.EncodeToString(digest[:]), nil
}

type migration struct {
	version int64
	up      string
	down    string
}

func loadMigrations(files fs.FS) ([]migration, error) {
	upFiles, err := fs.Glob(files, "*.up.sql")
	if err != nil {
		return nil, fmt.Errorf("list up migrations: %w", err)
	}
	migrations := make([]migration, 0, len(upFiles))
	for _, up := range upFiles {
		base := path.Base(up)
		versionText := strings.TrimSuffix(base, ".up.sql")
		if separator := strings.IndexByte(versionText, '_'); separator >= 0 {
			versionText = versionText[:separator]
		}
		version, err := strconv.ParseInt(versionText, 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration filename %q", base)
		}
		down := strings.TrimSuffix(up, ".up.sql") + ".down.sql"
		if _, err := fs.Stat(files, down); err != nil {
			return nil, fmt.Errorf("migration %d has no down file: %w", version, err)
		}
		migrations = append(migrations, migration{version: version, up: up, down: down})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for i := 1; i < len(migrations); i++ {
		if migrations[i-1].version == migrations[i].version {
			return nil, fmt.Errorf("duplicate migration version %d", migrations[i].version)
		}
	}
	return migrations, nil
}

func findMigration(migrations []migration, version int64) (migration, bool) {
	for _, migration := range migrations {
		if migration.version == version {
			return migration, true
		}
	}
	return migration{}, false
}
