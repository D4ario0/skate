package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/spf13/cobra"
)

func TestParseEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := `
# ignored
FOO=bar
EMPTY=
export BAZ="qux\nzap"
SINGLE='literal value'
QUOTED_COMMENT="quoted value" # comment
INLINE=value # comment
HASH=value#kept
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"FOO":            "bar",
		"EMPTY":          "",
		"BAZ":            "qux\nzap",
		"SINGLE":         "literal value",
		"QUOTED_COMMENT": "quoted value",
		"INLINE":         "value",
		"HASH":           "value#kept",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseEnvFileRejectsInvalidLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("NOPE\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := parseEnvFile(path); err == nil {
		t.Fatal("expected invalid line error")
	}
}

func TestKeyParserPreservesKeyCase(t *testing.T) {
	key, db, err := keyParser("DATABASE_URL@Boletteros")
	if err != nil {
		t.Fatal(err)
	}
	if string(key) != "DATABASE_URL" {
		t.Fatalf("key = %q, want %q", string(key), "DATABASE_URL")
	}
	if db != "boletteros" {
		t.Fatalf("db = %q, want %q", db, "boletteros")
	}
}

func TestSecretSetMasksListButGetAndEnvUseRealValue(t *testing.T) {
	withTempDataHome(t)
	resetCommandState(t)

	secretSet = true
	if err := set(&cobra.Command{}, []string{"token", "s3cr3t"}); err != nil {
		t.Fatal(err)
	}
	secretSet = false
	if err := set(&cobra.Command{}, []string{"plain", "visible"}); err != nil {
		t.Fatal(err)
	}

	gotList := captureStdout(t, func() {
		if err := list(&cobra.Command{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if gotList != "plain\tvisible\ntoken\t******\n" {
		t.Fatalf("list output = %q", gotList)
	}

	gotGet := captureStdout(t, func() {
		if err := get(&cobra.Command{}, []string{"token"}); err != nil {
			t.Fatal(err)
		}
	})
	if gotGet != "s3cr3t" {
		t.Fatalf("get output = %q, want %q", gotGet, "s3cr3t")
	}

	gotEnv, err := envFromDB("")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(gotEnv)
	wantEnv := []string{"plain=visible", "token=s3cr3t"}
	if strings.Join(gotEnv, "\n") != strings.Join(wantEnv, "\n") {
		t.Fatalf("env = %#v, want %#v", gotEnv, wantEnv)
	}
}

func TestNonSecretOverwriteClearsSecretMarker(t *testing.T) {
	withTempDataHome(t)
	resetCommandState(t)

	secretSet = true
	if err := set(&cobra.Command{}, []string{"token", "old"}); err != nil {
		t.Fatal(err)
	}
	secretSet = false
	if err := set(&cobra.Command{}, []string{"token", "new"}); err != nil {
		t.Fatal(err)
	}

	got := captureStdout(t, func() {
		if err := list(&cobra.Command{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if got != "token\tnew\n" {
		t.Fatalf("list output = %q, want %q", got, "token\tnew\n")
	}
}

func TestDeleteRemovesSecretMarker(t *testing.T) {
	withTempDataHome(t)
	resetCommandState(t)

	secretSet = true
	if err := set(&cobra.Command{}, []string{"token", "old"}); err != nil {
		t.Fatal(err)
	}
	if err := del(&cobra.Command{}, []string{"token"}); err != nil {
		t.Fatal(err)
	}

	db, err := openKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck

	err = db.View(func(tx *badger.Txn) error {
		_, err := tx.Get(secretMarkerKey([]byte("token")))
		return err
	})
	if !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("secret marker lookup error = %v, want %v", err, badger.ErrKeyNotFound)
	}
}

func TestSetFromEnvAlwaysMarksValuesAsSecrets(t *testing.T) {
	withTempDataHome(t)
	resetCommandState(t)

	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("TOKEN=abc123\nPLAIN=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFileSet = envPath
	if err := set(&cobra.Command{}, nil); err != nil {
		t.Fatal(err)
	}

	got := captureStdout(t, func() {
		if err := list(&cobra.Command{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if got != "PLAIN\t******\nTOKEN\t******\n" {
		t.Fatalf("list output = %q", got)
	}

	gotGet := captureStdout(t, func() {
		if err := get(&cobra.Command{}, []string{"TOKEN"}); err != nil {
			t.Fatal(err)
		}
	})
	if gotGet != "abc123" {
		t.Fatalf("get output = %q, want %q", gotGet, "abc123")
	}
}

func TestSetFromEnvUsesNamedDBAndLeavesDefaultEmpty(t *testing.T) {
	withTempDataHome(t)
	resetCommandState(t)

	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("TOKEN=abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFileSet = envPath
	if err := set(&cobra.Command{}, []string{"@work"}); err != nil {
		t.Fatal(err)
	}

	gotDefault := captureStdout(t, func() {
		if err := list(&cobra.Command{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if gotDefault != "" {
		t.Fatalf("default list output = %q, want empty", gotDefault)
	}

	gotWork := captureStdout(t, func() {
		if err := list(&cobra.Command{}, []string{"@work"}); err != nil {
			t.Fatal(err)
		}
	})
	if gotWork != "TOKEN\t******\n" {
		t.Fatalf("work list output = %q", gotWork)
	}
}

func TestListKeysOnlySkipsSecretMarkers(t *testing.T) {
	withTempDataHome(t)
	resetCommandState(t)

	secretSet = true
	if err := set(&cobra.Command{}, []string{"token", "s3cr3t"}); err != nil {
		t.Fatal(err)
	}
	keysIterate = true

	got := captureStdout(t, func() {
		if err := list(&cobra.Command{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if got != "token\n" {
		t.Fatalf("keys-only list output = %q, want %q", got, "token\n")
	}
}

func TestValuesOnlyMasksSecretValues(t *testing.T) {
	withTempDataHome(t)
	resetCommandState(t)

	secretSet = true
	if err := set(&cobra.Command{}, []string{"token", "s3cr3t"}); err != nil {
		t.Fatal(err)
	}
	valuesIterate = true

	got := captureStdout(t, func() {
		if err := list(&cobra.Command{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if got != "******\n" {
		t.Fatalf("values-only list output = %q, want %q", got, "******\n")
	}
}

func TestParseDBArgDefaultsAndRejectsKeyLikeArg(t *testing.T) {
	db, err := parseDBArg("")
	if err != nil {
		t.Fatal(err)
	}
	if db != "" {
		t.Fatalf("db = %q, want default", db)
	}

	db, err = parseDBArg("@Project")
	if err != nil {
		t.Fatal(err)
	}
	if db != "project" {
		t.Fatalf("db = %q, want %q", db, "project")
	}

	if _, err := parseDBArg("Project"); err == nil {
		t.Fatal("expected db format error")
	}
}

func withTempDataHome(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

func resetCommandState(t *testing.T) {
	t.Helper()
	reverseIterate = false
	keysIterate = false
	valuesIterate = false
	showBinary = false
	delimiterIterate = "\t"
	envFileSet = ""
	secretSet = false
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = oldStdout

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(out)
}
