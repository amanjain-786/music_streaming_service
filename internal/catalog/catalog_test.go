package catalog

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestScanDeduplicatesAndRetainsUnavailableLocations(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "root-a")
	rootB := filepath.Join(base, "root-b")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatal(err)
	}
	contents := []byte("same media bytes")
	fileA := filepath.Join(rootA, "song.mp3")
	fileB := filepath.Join(rootB, "copy.mp3")
	if err := os.WriteFile(fileA, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(base, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}

	result, err := Scan(ctx, db, []string{rootA, rootB})
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesHashed != 2 || result.FilesFound != 2 {
		t.Fatalf("first scan = %+v, want 2 files hashed and found", result)
	}
	if len(result.Duplicates) != 1 {
		t.Fatalf("first scan reported %d duplicates; want 1", len(result.Duplicates))
	}

	var songCount, locationCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM songs`).Scan(&songCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM media_locations`).Scan(&locationCount); err != nil {
		t.Fatal(err)
	}
	if songCount != 1 || locationCount != 2 {
		t.Fatalf("catalog has %d songs and %d locations; want 1 song and 2 locations", songCount, locationCount)
	}

	items, err := ListAvailable(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID == 0 {
		t.Fatalf("ListAvailable returned %+v; want one song with a stable ID", items)
	}

	result, err = Scan(ctx, db, []string{rootA, rootB})
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesHashed != 0 || result.FilesUnchanged != 2 {
		t.Fatalf("unchanged scan = %+v, want zero hashes and two unchanged files", result)
	}
	if len(result.Duplicates) != 1 {
		t.Fatalf("unchanged scan reported %d duplicates; want 1", len(result.Duplicates))
	}
	if err := os.WriteFile(fileA, []byte("changed media bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = Scan(ctx, db, []string{rootA, rootB})
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesHashed != 1 || result.FilesUnchanged != 1 {
		t.Fatalf("changed-file scan = %+v, want one hash and one unchanged file", result)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM songs`).Scan(&songCount); err != nil {
		t.Fatal(err)
	}
	if songCount != 2 {
		t.Fatalf("content change left %d song records; want 2 distinct identities", songCount)
	}

	if err := os.Remove(fileA); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fileB); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(ctx, db, []string{rootA, rootB}); err != nil {
		t.Fatal(err)
	}
	items, err = ListAvailable(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("ListAvailable returned %d songs after locations disappeared; want none", len(items))
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM media_locations WHERE is_available = 0`).Scan(&locationCount); err != nil {
		t.Fatal(err)
	}
	if locationCount != 2 {
		t.Fatalf("found %d unavailable locations; want 2 retained location rows", locationCount)
	}
}
