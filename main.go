package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var mediaRoots = []string{
	"D:/gaane_novideo",
	"D:/gaane",
	// Add more roots here if needed:
	// "D:/videos",
	// "E:/music",
}

const port = ":8080"

type Song struct {
	Title      string
	Initial    string
	RelPath    string
	Folder     string
	PlayCount  int
	Added      time.Time
	AddedLabel string
	LastPlayed time.Time
}

type PageData struct {
	Songs      []Song
	MostPlayed []Song
}

var db *sql.DB

func main() {
	var err error
	db, err = openDB("plays.db")
	if err != nil {
		fmt.Printf("Database failed: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/stream", streamHandler)
	http.HandleFunc("/played", playedHandler)

	fmt.Printf("[*] Music server started. Open http://localhost%s in your browser\n", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		fmt.Printf("Server failed: %v\n", err)
	}
}

func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		conn.Close()
		return nil, err
	}
	_, err = conn.Exec(`
		CREATE TABLE IF NOT EXISTS plays (
			rel_path       TEXT PRIMARY KEY,
			title          TEXT NOT NULL,
			play_count     INTEGER NOT NULL DEFAULT 0,
			last_played_at DATETIME
		)`)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func scanMusicFolder(root string) ([]Song, error) {
	var songs []Song

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		fullPath := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			subEntries, err := os.ReadDir(fullPath)
			if err != nil {
				continue
			}
			for _, subEntry := range subEntries {
				if subEntry.IsDir() || !isMediaFile(subEntry.Name()) {
					continue
				}
				info, _ := subEntry.Info()
				subFull := filepath.Join(fullPath, subEntry.Name())
				rel, _ := filepath.Rel(root, subFull)
				songs = append(songs, newSong(subEntry.Name(), mediaRelPath(root, rel), info))
			}
			continue
		}
		if isMediaFile(entry.Name()) {
			info, _ := entry.Info()
			rel, _ := filepath.Rel(root, fullPath)
			songs = append(songs, newSong(entry.Name(), mediaRelPath(root, rel), info))
		}
	}

	sort.Slice(songs, func(i, j int) bool {
		if songs[i].Added.Equal(songs[j].Added) {
			return songs[i].Title < songs[j].Title
		}
		return songs[i].Added.After(songs[j].Added)
	})
	return songs, nil
}

func scanAllMediaFolders() ([]Song, error) {
	if len(mediaRoots) == 0 {
		return nil, nil
	}

	var all []Song
	for _, root := range mediaRoots {
		items, err := scanMusicFolder(root)
		if err != nil {
			continue
		}
		all = append(all, items...)
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Added.Equal(all[j].Added) {
			return all[i].Title < all[j].Title
		}
		return all[i].Added.After(all[j].Added)
	})
	return all, nil
}

func mediaRelPath(root, rel string) string {
	cleaned := filepath.ToSlash(filepath.Clean(rel))
	if len(mediaRoots) <= 1 {
		return cleaned
	}

	rootName := strings.TrimSpace(filepath.Base(filepath.Clean(root)))
	if rootName == "" || rootName == "." {
		rootName = "root"
	}
	return rootName + "/" + strings.TrimPrefix(cleaned, "/")
}

func newSong(filename, rel string, info os.FileInfo) Song {
	added := createdAt(info)
	folder := ""
	dir := filepath.Dir(rel)
	if dir != "." {
		folder = filepath.ToSlash(dir)
	}
	title := strings.TrimSuffix(filename, filepath.Ext(filename))
	return Song{
		Title:      title,
		Initial:    initial(title),
		RelPath:    filepath.ToSlash(rel),
		Folder:     folder,
		Added:      added,
		AddedLabel: added.Format("02 Jan 2006"),
	}
}

func initial(title string) string {
	runes := []rune(strings.TrimSpace(title))
	if len(runes) == 0 {
		return "?"
	}
	return string(runes[0])
}

func createdAt(info os.FileInfo) time.Time {
	if info == nil {
		return time.Now()
	}
	if sys, ok := info.Sys().(*syscall.Win32FileAttributeData); ok && sys != nil {
		return time.Unix(0, sys.CreationTime.Nanoseconds())
	}
	return info.ModTime()
}

func isAudioFile(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mp3", ".flac", ".wav", ".m4a", ".aac":
		return true
	default:
		return false
	}
}

func isVideoFile(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi", ".wmv", ".flv", ".3gp":
		return true
	default:
		return false
	}
}

func isMediaFile(filename string) bool {
	return isAudioFile(filename) || isVideoFile(filename)
}

func attachPlays(songs []Song) ([]Song, error) {
	rows, err := db.Query(`SELECT rel_path, play_count, last_played_at FROM plays`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type stat struct {
		count int
		last  time.Time
	}
	byPath := map[string]stat{}
	for rows.Next() {
		var rel string
		var count int
		var last sql.NullString
		if err := rows.Scan(&rel, &count, &last); err != nil {
			return nil, err
		}
		item := stat{count: count}
		if last.Valid {
			if parsed, err := time.Parse("2006-01-02 15:04:05", last.String); err == nil {
				item.last = parsed
			}
		}
		fmt.Println("Loaded play count for", rel, ":", count, "last played:", item.last)
		byPath[filepath.ToSlash(rel)] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range songs {
		if i == 0 {
			fmt.Println(songs[i])
		}
		if item, ok := byPath[songs[i].RelPath]; ok {
			songs[i].PlayCount = item.count
			songs[i].LastPlayed = item.last
		}
	}
	return songs, nil
}

func mostPlayed(songs []Song) []Song {
	var out []Song
	for _, song := range songs {
		if song.PlayCount > 0 {
			out = append(out, song)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PlayCount == out[j].PlayCount {
			return out[i].LastPlayed.After(out[j].LastPlayed)
		}
		return out[i].PlayCount > out[j].PlayCount
	})
	return out
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	songs, err := scanAllMediaFolders()
	if err != nil {
		http.Error(w, fmt.Sprintf("Error scanning media folders: %v", err), http.StatusInternalServerError)
		return
	}
	songs, err = attachPlays(songs)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error reading play counts: %v", err), http.StatusInternalServerError)
		return
	}

	fmt.Println("[*] Serving index page with", len(songs), "songs")
	fmt.Println("[*] Most played songs:", len(mostPlayed(songs)))

	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("Template parsing error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, PageData{
		Songs:      songs,
		MostPlayed: mostPlayed(songs),
	}); err != nil {
		fmt.Printf("Template execute error: %v\n", err)
	}
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	full, _, err := safeMediaPath(r.URL.Query().Get("file"))
	if err != nil {
		http.Error(w, "Media file not found", http.StatusNotFound)
		return
	}

	file, err := os.Open(full)
	if err != nil {
		http.Error(w, "Media file not found", http.StatusNotFound)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		http.Error(w, "Media file not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", contentType(full))
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), file)
}

func playedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	_, rel, err := safeMediaPath(r.Form.Get("file"))
	if err != nil {
		http.Error(w, "Song file not found", http.StatusNotFound)
		return
	}

	title := strings.TrimSpace(r.Form.Get("title"))
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	}
	if len(title) > 300 {
		title = title[:300]
	}

	_, err = db.Exec(`
		INSERT INTO plays (rel_path, title, play_count, last_played_at)
		VALUES (?, ?, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(rel_path) DO UPDATE SET
			play_count = play_count + 1,
			title = excluded.title,
			last_played_at = CURRENT_TIMESTAMP
	`, rel, title)
	if err != nil {
		http.Error(w, "Could not record play", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func safeMediaPath(rel string) (string, string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || strings.Contains(rel, "\x00") {
		return "", "", fmt.Errorf("missing file")
	}

	normalized := filepath.ToSlash(filepath.Clean(strings.ReplaceAll(rel, "\\", "/")))
	normalized = strings.TrimPrefix(normalized, "/")
	if normalized == "." || strings.HasPrefix(normalized, "../") || strings.Contains(normalized, ":") {
		return "", "", fmt.Errorf("bad path")
	}

	for _, root := range mediaRoots {
		rootName := filepath.Base(filepath.Clean(root))
		prefix := strings.TrimSuffix(filepath.ToSlash(rootName), "/") + "/"
		if strings.HasPrefix(strings.ToLower(normalized), strings.ToLower(prefix)) {
			candidate := strings.TrimPrefix(normalized, prefix)
			return resolveMediaFile(root, candidate)
		}
	}

	for _, root := range mediaRoots {
		if full, relPath, err := resolveMediaFile(root, normalized); err == nil {
			return full, relPath, nil
		}
	}
	return "", "", fmt.Errorf("media not found")
}

func resolveMediaFile(root, rel string) (string, string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	candidate := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	if candidate == "." || strings.HasPrefix(candidate, "..") || strings.Contains(candidate, ":") {
		return "", "", fmt.Errorf("bad path")
	}

	full, err := filepath.Abs(filepath.Join(rootAbs, candidate))
	if err != nil {
		return "", "", err
	}
	prefix := strings.ToLower(rootAbs + string(os.PathSeparator))
	if !strings.HasPrefix(strings.ToLower(full), prefix) {
		return "", "", fmt.Errorf("path escapes root")
	}
	if !isMediaFile(full) {
		return "", "", fmt.Errorf("not media")
	}
	if _, err := os.Stat(full); err != nil {
		return "", "", err
	}
	stored := mediaRelPath(root, candidate)
	return full, stored, nil
}

func contentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac":
		return "audio/flac"
	case ".wav":
		return "audio/wav"
	case ".m4a", ".aac":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".flv":
		return "video/x-flv"
	default:
		return "application/octet-stream"
	}
}
