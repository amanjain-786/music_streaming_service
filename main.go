package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"musicstream/internal/catalog"
	appconfig "musicstream/internal/config"

	_ "modernc.org/sqlite"
)

var mediaRoots []string
var port string
var credentialsFile string

const sessionCookieName = "music_session"
const sessionLifetime = 24 * time.Hour
const loginRateLimit = 40
const loginRateWindow = time.Minute

type Song struct {
	ID              int64
	Title           string
	Initial         string
	RelPath         string
	Folder          string
	PlayCount       int
	GlobalPlayCount int
	Added           time.Time
	AddedLabel      string
	LastPlayed      time.Time
}

type PageData struct {
	Songs      []Song
	MostPlayed []Song
	Resume     *PlaybackState
}

type PlaybackState struct {
	SongID        int64
	RelPath       string
	Title         string
	Position      float64
	PositionLabel string
}

var db *sql.DB

type sessionData struct {
	username string
	expires  time.Time
}

type contextKey int

const authenticatedUsernameKey contextKey = iota

var sessions = struct {
	sync.Mutex
	items map[string]sessionData
}{items: make(map[string]sessionData)}
var loginRateLimits = struct {
	sync.Mutex
	clients     map[string]loginRateState
	lastCleanup time.Time
}{clients: make(map[string]loginRateState)}

type loginRateState struct {
	tokens    float64
	updatedAt time.Time
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "server"
	commandArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		commandArgs = args[1:]
	}
	if command != "server" && command != "scan" && command != "reset-play-data" {
		return fmt.Errorf("unknown command %q; use server, scan, or reset-play-data", command)
	}

	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to the shared JSON config")
	confirmReset := flags.Bool("yes", false, "confirm destructive play-data reset")
	if err := flags.Parse(commandArgs); err != nil {
		return err
	}
	if command == "reset-play-data" && !*confirmReset {
		return fmt.Errorf("reset-play-data deletes play counts and resume positions; rerun with --yes to confirm")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	cfg, err := appconfig.Load(*configPath)
	if err != nil {
		return err
	}
	mediaRoots = cfg.MediaRoots
	port = cfg.ListenAddress
	credentialsFile = cfg.Credentials

	db, err = openDB(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	if command == "reset-play-data" {
		if err := resetPlayData(context.Background(), db); err != nil {
			return err
		}
		fmt.Println("Play counts and resume positions were cleared; the catalog was preserved.")
		return nil
	}

	if command == "scan" {
		result, err := catalog.Scan(context.Background(), db, mediaRoots)
		if err != nil {
			return err
		}
		for _, duplicate := range result.Duplicates {
			fmt.Printf("Duplicate content %s: %q matches %q\n", duplicate.ContentHash, duplicate.Path, duplicate.OriginalPath)
		}
		fmt.Printf("Catalog scan complete: %d root(s), %d file(s), %d hashed, %d unchanged, %d unavailable location(s).\n",
			result.RootsScanned, result.FilesFound, result.FilesHashed,
			result.FilesUnchanged, result.LocationsMissing)
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/stream", streamHandler)
	mux.HandleFunc("/played", playedHandler)
	mux.HandleFunc("/progress", playbackProgressHandler)
	mux.HandleFunc("/login", loginHandler)
	mux.HandleFunc("/logout", logoutHandler)

	fmt.Printf("[*] Music server started. Open http://localhost%s in your browser\n", port)
	if err := http.ListenAndServe(port, authMiddleware(mux)); err != nil {
		return fmt.Errorf("server failed: %w", err)
	}
	return nil
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			if r.Method == http.MethodPost {
				allowed, retryAfter := allowLoginAttempt(r.RemoteAddr)
				if !allowed {
					seconds := int(retryAfter / time.Second)
					if retryAfter%time.Second != 0 {
						seconds++
					}
					if seconds < 1 {
						seconds = 1
					}
					w.Header().Set("Retry-After", fmt.Sprint(seconds))
					http.Error(w, "Too many login attempts", http.StatusTooManyRequests)
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		if username, ok := authenticatedUsername(r); ok {
			ctx := context.WithValue(r.Context(), authenticatedUsernameKey, username)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			target := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		http.Error(w, "Authentication required", http.StatusUnauthorized)
	})
}

func allowLoginAttempt(remoteAddr string) (bool, time.Duration) {
	clientIP, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		clientIP = remoteAddr
	}
	now := time.Now()

	loginRateLimits.Lock()
	defer loginRateLimits.Unlock()

	if now.Sub(loginRateLimits.lastCleanup) >= loginRateWindow {
		for ip, state := range loginRateLimits.clients {
			if now.Sub(state.updatedAt) >= loginRateWindow {
				delete(loginRateLimits.clients, ip)
			}
		}
		loginRateLimits.lastCleanup = now
	}

	state, ok := loginRateLimits.clients[clientIP]
	if !ok {
		state = loginRateState{tokens: loginRateLimit}
	} else {
		elapsed := now.Sub(state.updatedAt).Seconds()
		state.tokens += elapsed * loginRateLimit / loginRateWindow.Seconds()
		if state.tokens > loginRateLimit {
			state.tokens = loginRateLimit
		}
	}
	state.updatedAt = now
	if state.tokens < 1 {
		loginRateLimits.clients[clientIP] = state
		waitSeconds := (1 - state.tokens) * loginRateWindow.Seconds() / loginRateLimit
		return false, time.Duration(waitSeconds * float64(time.Second))
	}
	state.tokens--
	loginRateLimits.clients[clientIP] = state
	return true, 0
}

func authenticatedUsername(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}

	sessions.Lock()
	defer sessions.Unlock()
	session, ok := sessions.items[cookie.Value]
	if !ok {
		return "", false
	}
	if time.Now().After(session.expires) {
		delete(sessions.items, cookie.Value)
		return "", false
	}
	return session.username, true
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	next := safeNextPath(r.URL.Query().Get("next"))
	switch r.Method {
	case http.MethodGet:
		renderLogin(w, http.StatusOK, "", next)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			renderLogin(w, http.StatusBadRequest, "Invalid login request.", next)
			return
		}
		next = safeNextPath(r.Form.Get("next"))
		username := strings.TrimSpace(r.Form.Get("username"))
		password := r.Form.Get("password")
		ok, err := checkCredentials(username, password)
		if err != nil {
			fmt.Printf("Authentication configuration error: %v\n", err)
			http.Error(w, "Authentication is not configured. Check users.txt.", http.StatusServiceUnavailable)
			return
		}
		if !ok {
			renderLogin(w, http.StatusUnauthorized, "Invalid username or password.", next)
			return
		}

		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			http.Error(w, "Could not create session", http.StatusInternalServerError)
			return
		}
		sessionID := hex.EncodeToString(token)
		sessions.Lock()
		sessions.items[sessionID] = sessionData{
			username: username,
			expires:  time.Now().Add(sessionLifetime),
		}
		sessions.Unlock()

		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sessionID,
			Path:     "/",
			Expires:  time.Now().Add(sessionLifetime),
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func renderLogin(w http.ResponseWriter, status int, message, next string) {
	tmpl, err := template.ParseFiles("login.html")
	if err != nil {
		http.Error(w, "Login page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, struct {
		Message string
		Next    string
	}{Message: message, Next: next}); err != nil {
		fmt.Printf("Login template error: %v\n", err)
	}
}

func checkCredentials(username, password string) (bool, error) {
	if username == "" || password == "" {
		return false, nil
	}

	file, err := os.Open(credentialsFile)
	if err != nil {
		return false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	accountCount := 0
	matched := false
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		line = strings.TrimLeft(line, " \t")
		separator := strings.IndexAny(line, " \t")
		if separator <= 0 {
			return false, fmt.Errorf("invalid entry on line %d; expected username followed by a password", lineNumber)
		}
		storedUsername := line[:separator]
		storedPassword := strings.TrimLeft(line[separator:], " \t")
		if storedPassword == "" {
			return false, fmt.Errorf("invalid entry on line %d; expected username followed by a password", lineNumber)
		}
		accountCount++
		if storedUsername == username && subtle.ConstantTimeCompare([]byte(storedPassword), []byte(password)) == 1 {
			matched = true
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	if accountCount == 0 {
		return false, fmt.Errorf("no accounts configured")
	}
	return matched, nil
}

func safeNextPath(candidate string) string {
	if candidate == "" || !strings.HasPrefix(candidate, "/") || strings.HasPrefix(candidate, "//") || strings.Contains(candidate, "\\") {
		return "/"
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "/"
	}
	return candidate
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		sessions.Lock()
		delete(sessions.items, cookie.Value)
		sessions.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func openDB(path string) (*sql.DB, error) {
	databasePath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(databasePath, "/") {
		databasePath = "/" + databasePath
	}
	databaseURL := url.URL{Scheme: "file", Path: databasePath}
	query := databaseURL.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "foreign_keys(ON)")
	databaseURL.RawQuery = query.Encode()
	dsn := databaseURL.String()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		conn.Close()
		return nil, err
	}
	err = catalog.EnsureSchema(context.Background(), conn)
	if err == nil {
		_, err = conn.Exec(`
			CREATE TABLE IF NOT EXISTS song_play_counts (
				song_id        INTEGER PRIMARY KEY REFERENCES songs(id),
				play_count     INTEGER NOT NULL DEFAULT 0,
				last_played_at DATETIME
			);

			CREATE TABLE IF NOT EXISTS user_song_plays (
				username       TEXT NOT NULL,
				song_id        INTEGER NOT NULL REFERENCES songs(id),
				play_count     INTEGER NOT NULL DEFAULT 0,
				last_played_at DATETIME,
				PRIMARY KEY (username, song_id)
			);

			CREATE TABLE IF NOT EXISTS user_playback_state (
				username         TEXT PRIMARY KEY,
				song_id          INTEGER NOT NULL REFERENCES songs(id),
				position_seconds REAL NOT NULL DEFAULT 0,
				updated_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			);

			CREATE TABLE IF NOT EXISTS playlists (
				id         INTEGER PRIMARY KEY,
				name       TEXT NOT NULL,
				ownername  TEXT NOT NULL,
				created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			);

			CREATE TABLE IF NOT EXISTS playlist_songs (
				playlist_id INTEGER NOT NULL,
				song_id     INTEGER NOT NULL,
				position    INTEGER NOT NULL,

				PRIMARY KEY (playlist_id, song_id),

				FOREIGN KEY (playlist_id) REFERENCES playlists(id)
					ON DELETE CASCADE,

				FOREIGN KEY (song_id) REFERENCES songs(id)
			);
		`)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func resetPlayData(ctx context.Context, conn *sql.DB) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, table := range []string{"user_playback_state", "user_song_plays", "song_play_counts"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	for _, table := range []string{"playback_state", "user_plays", "plays"} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return fmt.Errorf("drop legacy table %s: %w", table, err)
		}
	}
	return tx.Commit()
}

func loadCatalogSongs() ([]Song, error) {
	media, err := catalog.ListAvailable(context.Background(), db)
	if err != nil {
		return nil, err
	}
	songs := make([]Song, 0, len(media))
	for _, item := range media {
		rel := mediaRelPath(item.RootPath, item.RelativePath)
		folder := filepath.ToSlash(filepath.Dir(rel))
		if folder == "." {
			folder = ""
		}
		songs = append(songs, Song{
			ID:         item.ID,
			Title:      item.Title,
			Initial:    initial(item.Title),
			RelPath:    rel,
			Folder:     folder,
			Added:      item.ModifiedAt,
			AddedLabel: item.ModifiedAt.Format("02 Jan 2006"),
		})
	}
	return songs, nil
}

func mediaRelPath(root, rel string) string {
	cleaned := filepath.ToSlash(filepath.Clean(rel))
	if len(mediaRoots) <= 1 {
		return cleaned
	}
	return mediaRootLabel(root) + "/" + strings.TrimPrefix(cleaned, "/")
}

func mediaRootLabel(root string) string {
	rootName := filepath.Base(filepath.Clean(root))
	if rootName == "" || rootName == "." || rootName == string(filepath.Separator) {
		rootName = "root"
	}
	for _, candidate := range mediaRoots {
		if !strings.EqualFold(filepath.Clean(candidate), filepath.Clean(root)) &&
			strings.EqualFold(filepath.Base(filepath.Clean(candidate)), rootName) {
			fingerprint := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(root))))
			return rootName + "-" + hex.EncodeToString(fingerprint[:4])
		}
	}
	return rootName
}

func initial(title string) string {
	runes := []rune(strings.TrimSpace(title))
	if len(runes) == 0 {
		return "?"
	}
	return string(runes[0])
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

func attachPlays(songs []Song, username string) ([]Song, error) {
	rows, err := db.Query(`SELECT song_id, play_count FROM song_play_counts`)
	if err != nil {
		return nil, err
	}
	globalCounts := map[int64]int{}
	for rows.Next() {
		var songID int64
		var count int
		if err := rows.Scan(&songID, &count); err != nil {
			rows.Close()
			return nil, err
		}
		globalCounts[songID] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = db.Query(`SELECT song_id, play_count, last_played_at FROM user_song_plays WHERE username = ?`, username)
	if err != nil {
		return nil, err
	}
	type userStat struct {
		count int
		last  time.Time
	}
	userCounts := map[int64]userStat{}
	for rows.Next() {
		var songID int64
		var count int
		var last sql.NullString
		if err := rows.Scan(&songID, &count, &last); err != nil {
			rows.Close()
			return nil, err
		}
		item := userStat{count: count}
		if last.Valid {
			if parsed, err := time.Parse("2006-01-02 15:04:05", last.String); err == nil {
				item.last = parsed
			}
		}
		userCounts[songID] = item
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	for i := range songs {
		songs[i].GlobalPlayCount = globalCounts[songs[i].ID]
		if item, ok := userCounts[songs[i].ID]; ok {
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

func loadPlaybackState(username string) (*PlaybackState, error) {
	var state PlaybackState
	var root, relativePath string
	err := db.QueryRow(`
		SELECT state.song_id, state.position_seconds, location.root_path, location.relative_path, songs.title
		FROM user_playback_state state
		JOIN songs ON songs.id = state.song_id
		JOIN media_locations location ON location.song_id = state.song_id AND location.is_available = 1
		WHERE state.username = ?
		ORDER BY location.id
		LIMIT 1
	`, username).Scan(&state.SongID, &state.Position, &root, &relativePath, &state.Title)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state.RelPath = mediaRelPath(root, relativePath)
	state.PositionLabel = formatPlaybackPosition(state.Position)
	return &state, nil
}

func formatPlaybackPosition(position float64) string {
	seconds := int(position)
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	songs, err := loadCatalogSongs()
	if err != nil {
		http.Error(w, fmt.Sprintf("Error reading media catalog: %v", err), http.StatusInternalServerError)
		return
	}
	username, ok := r.Context().Value(authenticatedUsernameKey).(string)
	if !ok || username == "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	songs, err = attachPlays(songs, username)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error reading play counts: %v", err), http.StatusInternalServerError)
		return
	}
	resume, err := loadPlaybackState(username)
	if err != nil {
		http.Error(w, "Error reading playback position", http.StatusInternalServerError)
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
		Resume:     resume,
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

	fullPath, _, err := safeMediaPath(r.Form.Get("file"))
	if err != nil {
		http.Error(w, "Song file not found", http.StatusNotFound)
		return
	}
	var songID int64
	if err := db.QueryRowContext(r.Context(), `
		SELECT song_id FROM media_locations
		WHERE file_path = ? AND is_available = 1
	`, filepath.Clean(fullPath)).Scan(&songID); err != nil {
		http.Error(w, "Song is not in the catalog", http.StatusNotFound)
		return
	}

	username, ok := r.Context().Value(authenticatedUsernameKey).(string)
	if !ok || username == "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		fmt.Printf("Could not begin play transaction for user %q: %v\n", username, err)
		http.Error(w, "Could not record play", http.StatusInternalServerError)
		return
	}
	_, err = tx.Exec(`
		INSERT INTO song_play_counts (song_id, play_count, last_played_at)
		VALUES (?, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(song_id) DO UPDATE SET
			play_count = play_count + 1,
			last_played_at = CURRENT_TIMESTAMP
	`, songID)
	if err == nil {
		_, err = tx.Exec(`
			INSERT INTO user_song_plays (username, song_id, play_count, last_played_at)
			VALUES (?, ?, 1, CURRENT_TIMESTAMP)
			ON CONFLICT(username, song_id) DO UPDATE SET
				play_count = play_count + 1,
				last_played_at = CURRENT_TIMESTAMP
		`, username, songID)
	}
	if err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			fmt.Printf("Could not roll back play transaction for user %q: %v\n", username, rollbackErr)
		}
		fmt.Printf("Could not write play count for user %q and song ID %d: %v\n", username, songID, err)
		http.Error(w, "Could not record play", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		fmt.Printf("Could not commit play transaction for user %q and song ID %d: %v\n", username, songID, err)
		http.Error(w, "Could not record play", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func playbackProgressHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	username, ok := r.Context().Value(authenticatedUsernameKey).(string)
	if !ok || username == "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	position, err := strconv.ParseFloat(r.Form.Get("position"), 64)
	if err != nil || math.IsNaN(position) || math.IsInf(position, 0) || position < 0 || position > 7*24*60*60 {
		http.Error(w, "Invalid playback position", http.StatusBadRequest)
		return
	}
	fullPath, _, err := safeMediaPath(r.Form.Get("file"))
	if err != nil {
		http.Error(w, "Media file not found", http.StatusNotFound)
		return
	}
	var songID int64
	if err := db.QueryRowContext(r.Context(), `
		SELECT song_id FROM media_locations
		WHERE file_path = ? AND is_available = 1
	`, filepath.Clean(fullPath)).Scan(&songID); err != nil {
		http.Error(w, "Media is not in the catalog", http.StatusNotFound)
		return
	}
	_, err = db.Exec(`
		INSERT INTO user_playback_state (username, song_id, position_seconds, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(username) DO UPDATE SET
			song_id = excluded.song_id,
			position_seconds = excluded.position_seconds,
			updated_at = CURRENT_TIMESTAMP
	`, username, songID, position)
	if err != nil {
		fmt.Printf("Could not save playback position for user %q: %v\n", username, err)
		http.Error(w, "Could not save playback position", http.StatusInternalServerError)
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

	if len(mediaRoots) > 1 {
		for _, root := range mediaRoots {
			prefix := strings.TrimSuffix(filepath.ToSlash(mediaRootLabel(root)), "/") + "/"
			if strings.HasPrefix(strings.ToLower(normalized), strings.ToLower(prefix)) {
				candidate := normalized[len(prefix):]
				return resolveMediaFile(root, candidate)
			}
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
