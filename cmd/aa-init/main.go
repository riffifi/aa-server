// aa-init explicitly initializes a new Go deployment. Existing key files are
// validated and preserved; replacement is never an automatic startup action.
package main

import (
	"aa-server/internal/cryptocompat"
	"aa-server/internal/postgres"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"github.com/jackc/pgx/v5"
	"os"
	"path/filepath"
)

func keyFile(path string, master bool, uid int) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("key path is not a regular file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if master {
			_, err = cryptocompat.NewFernet(string(data))
			if err != nil {
				return err
			}
			if uid >= 0 {
				return os.Chown(path, uid, uid)
			}
			return nil
		}
		if len(data) != 32 {
			return errors.New("signing key must contain 32 bytes")
		}
		if uid >= 0 {
			return os.Chown(path, uid, uid)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return err
	}
	if master {
		data = []byte(base64.URLEncoding.EncodeToString(data))
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if uid >= 0 {
		return file.Chown(uid, uid)
	}
	return nil
}
func run() error {
	masterSource := flag.String("master-source", "", "explicit preserved master key to copy without replacement")
	split := flag.Bool("split", false, "place session and identity keys in separate mount directories")
	admin := flag.Bool("admin", false, "initialize the panel password only if missing")
	roles := flag.Bool("roles", false, "create restricted PostgreSQL runtime roles")
	signingSource := flag.String("signing-source", "", "explicit preserved signing key to copy without replacement")
	directory := flag.String("directory", "", "explicit key directory for a new deployment")
	schema := flag.Bool("schema", false, "explicitly initialize PostgreSQL schema")
	uid := flag.Int("uid", -1, "owner uid/gid for newly created key files (Linux)")
	flag.Parse()
	if *directory == "" {
		return errors.New("-directory is required")
	}
	if (*masterSource == "") != (*signingSource == "") {
		return errors.New("both preserved key paths must be provided together")
	}
	if (*admin || *roles) && !*schema {
		return errors.New("-schema is required with -admin or -roles")
	}
	var store *postgres.Store
	var err error
	if *schema {
		store, err = postgres.Open(context.Background(), os.Getenv("DATABASE_URL"), 2)
		if err != nil {
			return err
		}
		defer store.Close()
	}
	if err := os.MkdirAll(*directory, 0700); err != nil {
		return err
	}
	if *uid >= 0 {
		if err := os.Chown(*directory, *uid, *uid); err != nil {
			return err
		}
	}
	keyPath := func(name string) string {
		if !*split {
			return filepath.Join(*directory, name)
		}
		folder := "sessions"
		if name == "signing.key" {
			folder = "identity"
		}
		return filepath.Join(*directory, folder, name)
	}
	if store != nil {
		var exists bool
		if err = store.Pool.QueryRow(context.Background(), "SELECT to_regclass('aa.accounts') IS NOT NULL").Scan(&exists); err != nil {
			return err
		}
		if exists {
			var data bool
			if err = store.Pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM aa.accounts) OR EXISTS(SELECT 1 FROM aa.config) OR EXISTS(SELECT 1 FROM aaid.users)").Scan(&data); err != nil {
				return err
			}
			if data {
				for name, source := range map[string]string{"master.key": *masterSource, "signing.key": *signingSource} {
					if _, err = os.Stat(keyPath(name)); os.IsNotExist(err) && source == "" {
						return errors.New("database contains existing data; restore both preserved keys instead of generating replacements")
					}
				}
			}
		}
	}
	if *split {
		for _, name := range []string{"master.key", "signing.key"} {
			if _, err := os.Stat(filepath.Join(*directory, name)); err == nil && *masterSource == "" {
				return errors.New("legacy root key files found; supply both preserved key sources explicitly")
			}
			folder := filepath.Dir(keyPath(name))
			if err := os.MkdirAll(folder, 0700); err != nil {
				return err
			}
			if *uid >= 0 {
				if err := os.Chown(folder, *uid, *uid); err != nil {
					return err
				}
			}
		}
	}
	if *masterSource != "" {
		for name, source := range map[string]string{"master.key": *masterSource, "signing.key": *signingSource} {
			data, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			if name == "master.key" {
				if _, err = cryptocompat.NewFernet(string(data)); err != nil {
					return err
				}
			} else if len(data) != 32 {
				return errors.New("preserved signing key must have 32 bytes")
			}
			target := keyPath(name)
			if current, err := os.ReadFile(target); err == nil {
				if !bytes.Equal(current, data) {
					return errors.New("target contains a different key; refusing to replace it")
				}
			} else if os.IsNotExist(err) {
				file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if err != nil {
					return err
				}
				_, writeErr := file.Write(data)
				closeErr := file.Close()
				if writeErr != nil {
					return writeErr
				}
				if closeErr != nil {
					return closeErr
				}
			} else {
				return err
			}
		}
	}
	for _, item := range []struct {
		name   string
		master bool
	}{{"master.key", true}, {"signing.key", false}} {
		if err := keyFile(keyPath(item.name), item.master, *uid); err != nil {
			return fmt.Errorf("%s: %w", item.name, err)
		}
	}
	if *schema {
		if err = store.Initialize(context.Background()); err != nil {
			return err
		}
		var encrypted string
		readErr := store.Pool.QueryRow(context.Background(), "SELECT secret FROM aa.accounts ORDER BY id LIMIT 1").Scan(&encrypted)
		if readErr != nil && !errors.Is(readErr, pgx.ErrNoRows) {
			return readErr
		}
		if readErr == nil {
			key, err := os.ReadFile(keyPath("master.key"))
			if err != nil {
				return err
			}
			cipher, err := cryptocompat.NewFernet(string(key))
			if err != nil {
				return err
			}
			if _, err = cipher.Open(encrypted); err != nil {
				return errors.New("preserved master key does not decrypt existing accounts")
			}
		}
		if *roles {
			if err = runtimeRoles(context.Background(), store); err != nil {
				return err
			}
		}
		if *admin {
			master, err := os.ReadFile(keyPath("master.key"))
			if err != nil {
				return err
			}
			vault, err := postgres.NewVault(store, string(master))
			if err != nil {
				return err
			}
			return vault.InitializeAdmin(context.Background(), os.Getenv("ADMIN_PASSWORD"))
		}
		return nil
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
