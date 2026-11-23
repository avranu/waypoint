package kernel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestInvalid(t *testing.T) {
	err := Invalid("bad %s %d", "x", 2)
	var ie *InvalidError
	if !errors.As(err, &ie) || ie.Msg != "bad x 2" {
		t.Fatalf("%#v", err)
	}
	if !IsInvalid(err) || !IsInvalid(fmt.Errorf("ctx: %w", err)) {
		t.Fatal("IsInvalid should be true, wrapped or not")
	}
	if IsInvalid(errors.New("other")) || IsInvalid(nil) {
		t.Fatal("IsInvalid should be false for other errors and nil")
	}
}

func TestNotFound(t *testing.T) {
	err := NotFound("issue")
	if !errors.Is(err, ErrNotFound) || err.Error() != "issue not found" {
		t.Fatalf("%v", err)
	}
}

func TestForbidden(t *testing.T) {
	err := Forbidden("no %s", "x")
	if err.Error() != "no x" || !errors.Is(err, ErrForbidden) {
		t.Fatalf("%v", err)
	}
	var fe *ForbiddenError
	if !errors.As(err, &fe) {
		t.Fatal("errors.As should find *ForbiddenError")
	}
}

func TestReadFault(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"wrapped canceled", fmt.Errorf("read: %w", context.Canceled), false},
		{"other", errors.New("disk"), true},
	} {
		if got := ReadFault(tc.err); got != tc.want {
			t.Errorf("%s: ReadFault = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMissingOrDenied(t *testing.T) {
	for _, e := range []error{ErrNotFound, ErrForbidden, ErrUnauthorized, sql.ErrNoRows} {
		if !MissingOrDenied(e) || !MissingOrDenied(fmt.Errorf("w: %w", e)) {
			t.Errorf("%v should be missing or denied, wrapped or not", e)
		}
	}
	if MissingOrDenied(ErrConflict) || MissingOrDenied(nil) {
		t.Error("conflict and nil are not missing or denied")
	}
}
