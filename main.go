package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
const credentialsFile = "users.txt"
const sessionCookieName = "music_session"
const sessionLifetime = 24 * time.Hour
const loginRateLimit = 40
const loginRateWindow = time.Minute

type Song struct {
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
	var err error
	db, err = openDB("plays.db")
	if err != nil {
		fmt.Printf("Database failed: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/stream", streamHandler)
	mux.HandleFunc("/played", playedHandler)
	mux.HandleFunc("/login", loginHandler)
	mux.HandleFunc("/logout", logoutHandler)

	fmt.Printf("[*] Music server started. Open http://localhost%s in your browser\n", port)
	if err := http.ListenAndServe(port, authMiddleware(mux)); err != nil {
		fmt.Printf("Server failed: %v\n", err)
	}
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
	if err == nil {
		_, err = conn.Exec(`
			CREATE TABLE IF NOT EXISTS user_plays (
				username       TEXT NOT NULL,
				rel_path       TEXT NOT NULL,
				title          TEXT NOT NULL,
				play_count     INTEGER NOT NULL DEFAULT 0,
				last_played_at DATETIME,
				PRIMARY KEY (username, rel_path)
			)`)
	}
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

func attachPlays(songs []Song, username string) ([]Song, error) {
	rows, err := db.Query(`SELECT rel_path, play_count FROM plays`)
	if err != nil {
		return nil, err
	}
	globalCounts := map[string]int{}
	for rows.Next() {
		var rel string
		var count int
		if err := rows.Scan(&rel, &count); err != nil {
			rows.Close()
			return nil, err
		}
		globalCounts[filepath.ToSlash(rel)] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = db.Query(`SELECT rel_path, play_count, last_played_at FROM user_plays WHERE username = ?`, username)
	if err != nil {
		return nil, err
	}
	type userStat struct {
		count int
		last  time.Time
	}
	userCounts := map[string]userStat{}
	for rows.Next() {
		var rel string
		var count int
		var last sql.NullString
		if err := rows.Scan(&rel, &count, &last); err != nil {
			rows.Close()
			return nil, err
		}
		item := userStat{count: count}
		if last.Valid {
			if parsed, err := time.Parse("2006-01-02 15:04:05", last.String); err == nil {
				item.last = parsed
			}
		}
		userCounts[filepath.ToSlash(rel)] = item
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	for i := range songs {
		path := filepath.ToSlash(songs[i].RelPath)
		songs[i].GlobalPlayCount = globalCounts[path]
		if item, ok := userCounts[path]; ok {
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
		INSERT INTO plays (rel_path, title, play_count, last_played_at)
		VALUES (?, ?, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(rel_path) DO UPDATE SET
			play_count = play_count + 1,
			title = excluded.title,
			last_played_at = CURRENT_TIMESTAMP
	`, rel, title)
	if err == nil {
		_, err = tx.Exec(`
			INSERT INTO user_plays (username, rel_path, title, play_count, last_played_at)
			VALUES (?, ?, ?, 1, CURRENT_TIMESTAMP)
			ON CONFLICT(username, rel_path) DO UPDATE SET
				play_count = play_count + 1,
				title = excluded.title,
				last_played_at = CURRENT_TIMESTAMP
		`, username, rel, title)
	}
	if err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			fmt.Printf("Could not roll back play transaction for user %q: %v\n", username, rollbackErr)
		}
		fmt.Printf("Could not write play count for user %q and path %q: %v\n", username, rel, err)
		http.Error(w, "Could not record play", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		fmt.Printf("Could not commit play transaction for user %q and path %q: %v\n", username, rel, err)
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
