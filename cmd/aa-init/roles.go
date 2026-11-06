package main

import (
	"aa-server/internal/postgres"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
)

func runtimeRoles(ctx context.Context, store *postgres.Store) error {
	for _, item := range []struct{ role, schema, env string }{{"sessions", "aa", "SESSIONS_DB_PASSWORD"}, {"aaid", "aaid", "IDENTITY_DB_PASSWORD"}} {
		password := os.Getenv(item.env)
		if len(password) < 32 {
			return errors.New("runtime database passwords must contain at least 32 characters")
		}
		var exists bool
		if err := store.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", item.role).Scan(&exists); err != nil {
			return err
		}
		role := pgx.Identifier{item.role}.Sanitize()
		schema := pgx.Identifier{item.schema}.Sanitize()
		if !exists {
			if _, err := store.Pool.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '"+strings.ReplaceAll(password, "'", "''")+"'"); err != nil {
				return errors.New("could not create restricted database role")
			}
		}
		var unsafe bool
		if err := store.Pool.QueryRow(ctx, `SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR EXISTS(SELECT 1 FROM pg_auth_members m WHERE m.member=r.oid) FROM pg_roles r WHERE rolname=$1`, item.role).Scan(&unsafe); err != nil {
			return err
		}
		if unsafe {
			return errors.New("existing runtime database role is privileged; refusing to reuse it")
		}
		for _, sql := range []string{"REVOKE ALL ON SCHEMA " + schema + " FROM PUBLIC", "GRANT USAGE ON SCHEMA " + schema + " TO " + role, "GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA " + schema + " TO " + role, "GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA " + schema + " TO " + role} {
			if _, err := store.Pool.Exec(ctx, sql); err != nil {
				return err
			}
		}
	}
	return nil
}
