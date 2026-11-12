package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestScanCommandUsesSharedConfigAndDatabase(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "media")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.mp3"), []byte("sample media"), 0o600); err != nil {
		t.Fatal(err)
	}

	type testConfig struct {
		MediaRoots    []string `json:"media_roots"`
		DatabasePath  string   `json:"database_path"`
		Credentials   string   `json:"credentials_file"`
		ListenAddress string   `json:"listen_address"`
	}
	configBytes, err := json.Marshal(testConfig{
		MediaRoots:    []string{root},
		DatabasePath:  "catalog.db",
		Credentials:   "users.txt",
		ListenAddress: ":8080",
	})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "config.json")
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"scan", "--config", configPath}); err != nil {
		t.Fatal(err)
	}

	conn, err := openDB(filepath.Join(base, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var songCount, locationCount int
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM songs`).Scan(&songCount); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM media_locations WHERE is_available = 1`).Scan(&locationCount); err != nil {
		t.Fatal(err)
	}
	if songCount != 1 || locationCount != 1 {
		t.Fatalf("scan created %d songs and %d available locations; want 1 each", songCount, locationCount)
	}
}

func TestResetPlayDataClearsCountsAndPreservesCatalog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reset.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	result, err := db.Exec(`INSERT INTO songs (content_hash, title) VALUES ('hash', 'track')`)
	if err != nil {
		t.Fatal(err)
	}
	songID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO song_play_counts (song_id, play_count) VALUES (1, 3)`,
		`INSERT INTO user_song_plays (username, song_id, play_count) VALUES ('user', 1, 2)`,
		`INSERT INTO user_playback_state (username, song_id, position_seconds) VALUES ('user', 1, 45)`,
		`CREATE TABLE plays (rel_path TEXT PRIMARY KEY)`,
		`CREATE TABLE user_plays (rel_path TEXT PRIMARY KEY)`,
		`CREATE TABLE playback_state (rel_path TEXT PRIMARY KEY)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := resetPlayData(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"song_play_counts", "user_song_plays", "user_playback_state"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("table %s has %d rows after reset; want 0", table, count)
		}
	}
	var legacyTables int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN ('plays', 'user_plays', 'playback_state')
	`).Scan(&legacyTables); err != nil {
		t.Fatal(err)
	}
	if legacyTables != 0 {
		t.Fatalf("found %d legacy tables after reset; want none", legacyTables)
	}
	var catalogCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM songs WHERE id = ?`, songID).Scan(&catalogCount); err != nil {
		t.Fatal(err)
	}
	if catalogCount != 1 {
		t.Fatal("reset removed the song catalog")
	}
}
