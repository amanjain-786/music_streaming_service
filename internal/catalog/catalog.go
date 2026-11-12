package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Media struct {
	ID           int64
	ContentHash  string
	Title        string
	RootPath     string
	RelativePath string
	FilePath     string
	SizeBytes    int64
	ModifiedAt   time.Time
}

type ScanResult struct {
	RootsScanned     int
	FilesFound       int
	FilesHashed      int
	FilesUnchanged   int
	LocationsUpdated int
	LocationsMissing int64
	Duplicates       []Duplicate
}

type Duplicate struct {
	Path         string
	OriginalPath string
	ContentHash  string
}

type discoveredFile struct {
	media       Media
	contentHash string
	changed     bool
}

func EnsureSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS songs (
			id           INTEGER PRIMARY KEY,
			content_hash TEXT NOT NULL UNIQUE,
			title        TEXT NOT NULL,
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS media_locations (
			id             INTEGER PRIMARY KEY,
			song_id        INTEGER NOT NULL REFERENCES songs(id),
			file_path      TEXT NOT NULL UNIQUE,
			root_path      TEXT NOT NULL,
			relative_path  TEXT NOT NULL,
			size_bytes     INTEGER NOT NULL,
			modified_at_ns INTEGER NOT NULL,
			is_available   INTEGER NOT NULL DEFAULT 1,
			last_seen_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE INDEX IF NOT EXISTS idx_media_locations_song_available
			ON media_locations(song_id, is_available);
	`)
	if err != nil {
		return fmt.Errorf("create catalog schema: %w", err)
	}
	return nil
}

func Scan(ctx context.Context, db *sql.DB, roots []string) (ScanResult, error) {
	if len(roots) == 0 {
		return ScanResult{}, fmt.Errorf("no media roots configured")
	}
	if err := EnsureSchema(ctx, db); err != nil {
		return ScanResult{}, err
	}
	firstPathByHash, err := knownContentPaths(ctx, db)
	if err != nil {
		return ScanResult{}, err
	}

	var result ScanResult
	for _, configuredRoot := range roots {
		root, err := filepath.Abs(configuredRoot)
		if err != nil {
			return result, fmt.Errorf("resolve media root %q: %w", configuredRoot, err)
		}
		discovered, duplicates, err := discoverRoot(ctx, db, filepath.Clean(root), firstPathByHash)
		if err != nil {
			return result, fmt.Errorf("scan media root %q: %w", root, err)
		}
		result.Duplicates = append(result.Duplicates, duplicates...)
		missing, err := persistRoot(ctx, db, filepath.Clean(root), discovered)
		if err != nil {
			return result, fmt.Errorf("update catalog for root %q: %w", root, err)
		}
		result.RootsScanned++
		result.FilesFound += len(discovered)
		result.LocationsUpdated += len(discovered)
		result.LocationsMissing += missing
		for _, file := range discovered {
			if file.changed {
				result.FilesHashed++
			} else {
				result.FilesUnchanged++
			}
		}
	}
	return result, nil
}

func knownContentPaths(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.content_hash, MIN(l.file_path)
		FROM songs s
		JOIN media_locations l ON l.song_id = s.id
		WHERE l.is_available = 1
		GROUP BY s.content_hash
	`)
	if err != nil {
		return nil, fmt.Errorf("read known content hashes: %w", err)
	}
	defer rows.Close()

	paths := make(map[string]string)
	for rows.Next() {
		var hash, path string
		if err := rows.Scan(&hash, &path); err != nil {
			return nil, err
		}
		paths[hash] = path
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

func discoverRoot(ctx context.Context, db *sql.DB, root string, firstPathByHash map[string]string) ([]discoveredFile, []Duplicate, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("not a directory")
	}

	var discovered []discoveredFile
	var duplicates []Duplicate
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !IsMediaFile(entry.Name()) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		absolutePath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		relativePath, err := filepath.Rel(root, absolutePath)
		if err != nil {
			return err
		}
		media := Media{
			RootPath:     root,
			RelativePath: filepath.ToSlash(relativePath),
			FilePath:     filepath.Clean(absolutePath),
			Title:        strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
			SizeBytes:    fileInfo.Size(),
			ModifiedAt:   fileInfo.ModTime(),
		}

		var existingSongID int64
		var existingSize, existingModified int64
		lookupErr := db.QueryRowContext(ctx, `
			SELECT l.song_id, l.size_bytes, l.modified_at_ns, s.content_hash
			FROM media_locations l
			JOIN songs s ON s.id = l.song_id
			WHERE file_path = ?
		`, media.FilePath).Scan(&existingSongID, &existingSize, &existingModified, &media.ContentHash)
		if lookupErr != nil && lookupErr != sql.ErrNoRows {
			return lookupErr
		}
		if lookupErr == nil && existingSize == media.SizeBytes && existingModified == media.ModifiedAt.UnixNano() {
			media.ID = existingSongID
			if duplicate := registerContentPath(media.ContentHash, media.FilePath, firstPathByHash); duplicate != nil {
				duplicates = append(duplicates, *duplicate)
			}
			discovered = append(discovered, discoveredFile{media: media, contentHash: media.ContentHash})
			return nil
		}

		hash, err := hashFile(media.FilePath)
		if err != nil {
			return err
		}
		postHashInfo, err := os.Stat(media.FilePath)
		if err != nil {
			return err
		}
		if postHashInfo.Size() != media.SizeBytes || postHashInfo.ModTime().UnixNano() != media.ModifiedAt.UnixNano() {
			return fmt.Errorf("file changed while hashing: %q", media.FilePath)
		}
		discovered = append(discovered, discoveredFile{
			media:       media,
			contentHash: hash,
			changed:     true,
		})
		if duplicate := registerContentPath(hash, media.FilePath, firstPathByHash); duplicate != nil {
			duplicates = append(duplicates, *duplicate)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return discovered, duplicates, nil
}

func registerContentPath(hash, path string, firstPathByHash map[string]string) *Duplicate {
	if firstPath, exists := firstPathByHash[hash]; exists {
		if filepath.Clean(firstPath) == filepath.Clean(path) {
			return nil
		}
		return &Duplicate{Path: path, OriginalPath: firstPath, ContentHash: hash}
	}
	firstPathByHash[hash] = path
	return nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func persistRoot(ctx context.Context, db *sql.DB, root string, files []discoveredFile) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE media_locations SET is_available = 0 WHERE root_path = ?`, root); err != nil {
		return 0, err
	}

	for i := range files {
		file := &files[i]
		if file.changed {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO songs (content_hash, title)
				VALUES (?, ?)
				ON CONFLICT(content_hash) DO NOTHING
			`, file.contentHash, file.media.Title); err != nil {
				return 0, err
			}
			if err := tx.QueryRowContext(ctx, `
				SELECT id FROM songs WHERE content_hash = ?
			`, file.contentHash).Scan(&file.media.ID); err != nil {
				return 0, err
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO media_locations (
				song_id, file_path, root_path, relative_path, size_bytes,
				modified_at_ns, is_available, last_seen_at
			)
			VALUES (?, ?, ?, ?, ?, ?, 1, CURRENT_TIMESTAMP)
			ON CONFLICT(file_path) DO UPDATE SET
				song_id = excluded.song_id,
				root_path = excluded.root_path,
				relative_path = excluded.relative_path,
				size_bytes = excluded.size_bytes,
				modified_at_ns = excluded.modified_at_ns,
				is_available = 1,
				last_seen_at = CURRENT_TIMESTAMP
		`, file.media.ID, file.media.FilePath, root, file.media.RelativePath, file.media.SizeBytes, file.media.ModifiedAt.UnixNano()); err != nil {
			return 0, err
		}
	}

	var missing int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM media_locations
		WHERE root_path = ? AND is_available = 0
	`, root).Scan(&missing); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return missing, nil
}

func ListAvailable(ctx context.Context, db *sql.DB) ([]Media, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, s.content_hash, s.title, l.root_path, l.relative_path,
		       l.file_path, l.size_bytes, l.modified_at_ns
		FROM songs s
		JOIN media_locations l ON l.id = (
			SELECT MIN(l2.id)
			FROM media_locations l2
			WHERE l2.song_id = s.id AND l2.is_available = 1
		)
		WHERE EXISTS (
			SELECT 1 FROM media_locations available
			WHERE available.song_id = s.id AND available.is_available = 1
		)
		ORDER BY l.modified_at_ns DESC, s.title COLLATE NOCASE
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var media []Media
	for rows.Next() {
		var item Media
		var modifiedAt int64
		if err := rows.Scan(&item.ID, &item.ContentHash, &item.Title, &item.RootPath, &item.RelativePath, &item.FilePath, &item.SizeBytes, &modifiedAt); err != nil {
			return nil, err
		}
		item.ModifiedAt = time.Unix(0, modifiedAt)
		media = append(media, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return media, nil
}

func IsMediaFile(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mp3", ".flac", ".wav", ".m4a", ".aac", ".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi", ".wmv", ".flv", ".3gp":
		return true
	default:
		return false
	}
}
