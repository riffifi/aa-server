package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Open(ctx, url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var name string
	if err = s.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "aa_test_") {
		t.Fatal("integration tests require a dedicated aa_test_* database")
	}
	if err = s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Initialize(ctx); err != nil {
		t.Fatalf("schema initialization must be idempotent: %v", err)
	}
	return s
}

func TestPostgresExistingAccountShapeAndFilteredCursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := fmt.Sprintf("synthetic-%d", time.Now().UnixNano())
	defer s.Write(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM aa.events WHERE account_id=$1;", id)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM aa.accounts WHERE id=$1", id)
		return err
	})
	var eventID int64
	err := s.Write(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO aa.accounts(id,pulse_id,label,group_id,secret,state,checked_at,version,updated_at,pass_allowed)
   VALUES($1,$1,'Студент',5330,'encrypted','valid',1000,1,1000,0)`, id)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, "INSERT INTO aa.events(account_id,version,kind,created,group_id) VALUES($1,1,'upsert',1000,5330) RETURNING id", id).Scan(&eventID)
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.Account(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	public := Public(row)
	if public["label"] != "Студент" || public["pass_allowed"] != false {
		t.Fatalf("account shape changed: %#v", public)
	}
	if _, ok := public["secret"]; ok {
		t.Fatal("secret leaked into account metadata")
	}
	other := int64(12345)
	events, cursor, err := s.Events(ctx, eventID-1, &other)
	if err != nil || len(events) != 0 || cursor != eventID {
		t.Fatalf("filtered cursor failed: %d %v", cursor, err)
	}
	group := int64(5330)
	events, cursor, err = s.Events(ctx, eventID-1, &group)
	if err != nil || len(events) != 1 || cursor != eventID {
		t.Fatalf("event missing: %v", err)
	}
}

func TestPostgresPublishersCannotCommitPastAnUncommittedEvent(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := fmt.Sprintf("publisher-%d", time.Now().UnixNano())
	defer s.Write(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "DELETE FROM aa.events WHERE account_id=$1", id)
		return err
	})
	inserted, release, secondEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	var firstID, secondID int64
	go func() {
		firstDone <- s.Write(ctx, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, "INSERT INTO aa.events(account_id,version,kind,created) VALUES($1,1,'first',1000) RETURNING id", id).Scan(&firstID)
			close(inserted)
			if err != nil {
				return err
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-inserted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		secondDone <- s.Write(ctx, func(tx pgx.Tx) error {
			close(secondEntered)
			return tx.QueryRow(ctx, "INSERT INTO aa.events(account_id,version,kind,created) VALUES($1,2,'second',1001) RETURNING id", id).Scan(&secondID)
		})
	}()
	select {
	case <-secondEntered:
		close(release)
		t.Fatal("publisher bypassed transaction ordering lock")
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if secondID <= firstID {
		t.Fatal("event IDs are out of order")
	}
}
