package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInitializationPreservesExistingKeys(t *testing.T) {
	dir := t.TempDir()
	for _, master := range []bool{true, false} {
		path := filepath.Join(dir, "signing.key")
		if master {
			path = filepath.Join(dir, "master.key")
		}
		if err := keyFile(path, master, -1); err != nil {
			t.Fatal(err)
		}
		first, _ := os.ReadFile(path)
		if err := keyFile(path, master, -1); err != nil {
			t.Fatal(err)
		}
		second, _ := os.ReadFile(path)
		if !bytes.Equal(first, second) {
			t.Fatal("key rotated on restart")
		}
		if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := keyFile(path, master, -1); err == nil {
			t.Fatal("corrupt existing key replaced")
		}
	}
}
