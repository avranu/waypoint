package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListTestsHandlesTestMainAndParamTypes(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import (
	"testing"
	tst "testing"
)

func TestMain(m *testing.M) {}
func TestOrdinary(t *testing.T) {}
func TestAliased(t *tst.T) {}
func TestHelper(x int) {}
func Testlowercase(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(dir, "a_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := captureListTests(t, dir)
	if want := "TestAliased\nTestOrdinary\n"; got != want {
		t.Fatalf("with TestMain(*testing.M): got %q, want %q", got, want)
	}

	dir2 := t.TempDir()
	src2 := `package sample

import "testing"

func TestMain(t *testing.T) {}
func TestOther(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(dir2, "b_test.go"), []byte(src2), 0o600); err != nil {
		t.Fatal(err)
	}
	got2 := captureListTests(t, dir2)
	if want := "TestMain\nTestOther\n"; got2 != want {
		t.Fatalf("with TestMain(*testing.T): got %q, want %q", got2, want)
	}
}

func captureListTests(t *testing.T, dir string) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := listTests(dir)
	_ = w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	if runErr != nil {
		t.Fatalf("listTests(%q): %v", dir, runErr)
	}
	return strings.ReplaceAll(buf.String(), "\r\n", "\n")
}
