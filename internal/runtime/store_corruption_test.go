package runtime

import (
	"fmt"
	"testing"
)

type sqliteResultError int

func (e sqliteResultError) Error() string { return "sqlite failure" }
func (e sqliteResultError) Code() int     { return int(e) }

func TestMigrationFailureIsNotMistakenForCorruption(t *testing.T) {
	for _, code := range []int{5, 8, 13, 19} {
		if isSQLiteCorruption(fmt.Errorf("create index: %w", sqliteResultError(code))) {
			t.Fatalf("code %d must not quarantine history", code)
		}
	}
	for _, code := range []int{11, 26, 11 | (1 << 8)} {
		if !isSQLiteCorruption(fmt.Errorf("open: %w", sqliteResultError(code))) {
			t.Fatalf("code %d should report corruption", code)
		}
	}
}
