package server

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canonicalUserAssetsCommit = "9cbb06b8b649ebc4a3ad818e56e945ce9965c141"

func TestVendoredUserAssetFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "protocols", "fixtures")
	for _, dir := range []string{"user-assets", "runtime-gateway-user-assets"} {
		t.Run(dir, func(t *testing.T) { verifyFixtureVendor(t, filepath.Join(root, dir)) })
	}
}

func verifyFixtureVendor(t *testing.T, dir string) {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(dir, "SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "canonical_commit="+canonicalUserAssetsCommit) {
		t.Fatalf("SOURCE does not pin canonical commit: %s", source)
	}
	manifest, err := os.Open(filepath.Join(dir, "manifest.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()
	scanner := bufio.NewScanner(manifest)
	entries := 0
	for scanner.Scan() {
		var want, name string
		if _, err := fmt.Sscan(scanner.Text(), &want, &name); err != nil {
			t.Fatalf("bad manifest line %q: %v", scanner.Text(), err)
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		got := fmt.Sprintf("%x", sha256.Sum256(data))
		if got != want {
			t.Fatalf("%s checksum=%s want=%s", name, got, want)
		}
		entries++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if entries == 0 {
		t.Fatal("empty fixture manifest")
	}
}
