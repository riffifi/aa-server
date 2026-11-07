package postgres

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
)

var importTables = []string{"aa.config", "aa.accounts", "aa.audit_events", "aa.panel_sessions", "aa.events", "aa.telegram_links", "aa.site_sessions", "aa.marks", "aa.pass_shares", "aa.telegram_users", "aa.telegram_codes", "aa.share_requests", "aa.pass_clients", "aa.pass_requests", "aa.pass_grants", "aaid.users", "aaid.mirea_links", "aaid.sessions", "aaid.refresh_tokens", "aaid.web_tokens", "aaid.audit", "aa_legacy.devices"}

type importRecord struct {
	Type    string            `json:"type"`
	Version int               `json:"version"`
	Schema  string            `json:"schema"`
	Name    string            `json:"name"`
	Columns []string          `json:"columns"`
	Values  []json.RawMessage `json:"values"`
	Counts  map[string]int64  `json:"counts"`
	SHA256  string            `json:"sha256"`
}

// ImportSnapshot imports only into an empty target and rolls back every row on
// malformed input, unknown columns, a checksum mismatch, or a constraint failure.
// The encryption/signing key files are deliberately not regenerated or imported.
func (s *Store) ImportSnapshot(ctx context.Context, input io.Reader) (map[string]int64, error) {
	if err := s.Initialize(ctx); err != nil {
		return nil, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(6067701)"); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(6067702)"); err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	quoted := make([]string, 0, len(importTables))
	for _, table := range importTables {
		allowed[table] = true
		quoted = append(quoted, pgx.Identifier(strings.Split(table, ".")).Sanitize())
	}
	if _, err = tx.Exec(ctx, "LOCK TABLE "+strings.Join(quoted, ",")+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return nil, err
	}
	for _, table := range quoted {
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+")").Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			return nil, errors.New("import target is not empty")
		}
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 65536), 16*1024*1024)
	checksum := sha256.New()
	counts := map[string]int64{}
	seen := map[string]bool{}
	table := ""
	var columns []string
	var types []string
	batch := [][]any{}
	batchBytes := 0
	header, summary := false, false
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := tx.CopyFrom(ctx, pgx.Identifier(strings.Split(table, ".")), columns, pgx.CopyFromRows(batch))
		if err != nil {
			return fmt.Errorf("import %s failed: %w", table, err)
		}
		counts[table] += n
		batch = nil
		batchBytes = 0
		return nil
	}
	for scanner.Scan() {
		raw := scanner.Bytes()
		var record importRecord
		if err = json.Unmarshal(raw, &record); err != nil {
			return nil, errors.New("invalid snapshot record")
		}
		if summary {
			return nil, errors.New("data after snapshot summary")
		}
		if !header && record.Type != "header" {
			return nil, errors.New("missing snapshot header")
		}
		if record.Type != "summary" {
			checksum.Write(raw)
			checksum.Write([]byte{'\n'})
		}
		switch record.Type {
		case "header":
			if header || record.Version != 1 {
				return nil, errors.New("unsupported or duplicate snapshot header")
			}
			header = true
		case "table":
			if err = flush(); err != nil {
				return nil, err
			}
			table = record.Schema + "." + record.Name
			if !allowed[table] || seen[table] || len(record.Columns) == 0 {
				return nil, errors.New("invalid or duplicate snapshot table")
			}
			seen[table] = true
			counts[table] = 0
			rows, err := tx.Query(ctx, "SELECT column_name,data_type FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2", record.Schema, record.Name)
			if err != nil {
				return nil, err
			}
			available := map[string]string{}
			for rows.Next() {
				var name, kind string
				if err = rows.Scan(&name, &kind); err != nil {
					rows.Close()
					return nil, err
				}
				available[name] = kind
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			columns = record.Columns
			types = make([]string, len(columns))
			seenColumns := map[string]bool{}
			for i, column := range columns {
				kind, ok := available[column]
				if !ok || seenColumns[column] {
					return nil, fmt.Errorf("unsupported column in %s", table)
				}
				seenColumns[column] = true
				types[i] = kind
			}
		case "row":
			if table == "" || len(record.Values) != len(columns) {
				return nil, errors.New("invalid snapshot row")
			}
			values := make([]any, len(columns))
			for i, value := range record.Values {
				if string(value) == "null" {
					continue
				}
				switch types[i] {
				case "bigint":
					var number int64
					if json.Unmarshal(value, &number) != nil {
						return nil, fmt.Errorf("invalid integer in %s", table)
					}
					values[i] = number
				case "double precision":
					var number float64
					if json.Unmarshal(value, &number) != nil {
						return nil, fmt.Errorf("invalid timestamp in %s", table)
					}
					values[i] = number
				case "text":
					var text string
					if json.Unmarshal(value, &text) != nil {
						return nil, fmt.Errorf("invalid text in %s", table)
					}
					values[i] = text
				default:
					return nil, fmt.Errorf("unsupported PostgreSQL type in %s", table)
				}
			}
			batch = append(batch, values)
			batchBytes += len(raw)
			if len(batch) >= 100 || batchBytes >= 8*1024*1024 {
				if err = flush(); err != nil {
					return nil, err
				}
			}
		case "summary":
			if err = flush(); err != nil {
				return nil, err
			}
			if record.SHA256 != hex.EncodeToString(checksum.Sum(nil)) || !reflect.DeepEqual(record.Counts, counts) {
				return nil, errors.New("snapshot checksum or row counts do not match")
			}
			if !seen["aa.accounts"] || !seen["aa.config"] || !seen["aa.events"] || !seen["aaid.users"] || !seen["aaid.mirea_links"] || !seen["aaid.sessions"] || !seen["aaid.refresh_tokens"] {
				return nil, errors.New("snapshot omits required tables")
			}
			summary = true
		default:
			return nil, errors.New("unknown snapshot record")
		}
	}
	if err = scanner.Err(); err != nil {
		return nil, errors.New("snapshot could not be read or a record exceeds 16 MiB")
	}
	if !header || !summary {
		return nil, errors.New("incomplete snapshot")
	}
	for _, table := range []string{"aa.audit_events", "aa.events", "aa.marks", "aaid.audit"} {
		name := pgx.Identifier(strings.Split(table, ".")).Sanitize()
		if _, err = tx.Exec(ctx, "SELECT setval(pg_get_serial_sequence($1,'id'),COALESCE(MAX(id),1),COUNT(*)>0) FROM "+name, table); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return counts, nil
}
