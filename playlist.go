package main

// Playlist endpoints. They use the `playlists` and `playlist_songs` tables
// that openDB() already creates, so no migration is needed.
//
//   GET  /playlists          -> all of the logged-in user's playlists, with songs
//   POST /playlists/create   -> name
//   POST /playlists/delete   -> playlist_id
//   POST /playlists/add      -> playlist_id, file
//   POST /playlists/remove   -> playlist_id, song_id

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

type PlaylistSong struct {
	ID    int64  `json:"id"`
	File  string `json:"file"` // same value as data-file in the page (Song.RelPath)
	Title string `json:"title"`
}

type Playlist struct {
	ID    int64          `json:"id"`
	Name  string         `json:"name"`
	Songs []PlaylistSong `json:"songs"`
}

// registerPlaylistRoutes wires everything up; call it from run() next to the other mux.HandleFunc lines.
func registerPlaylistRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/playlists", playlistsListHandler)
	mux.HandleFunc("/playlists/create", playlistCreateHandler)
	mux.HandleFunc("/playlists/delete", playlistDeleteHandler)
	mux.HandleFunc("/playlists/add", playlistAddSongHandler)
	mux.HandleFunc("/playlists/remove", playlistRemoveSongHandler)
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Printf("JSON encode error: %v\n", err)
	}
}

// postUser checks the method, parses the form and returns the logged-in username.
func postUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return "", false
	}
	username, ok := r.Context().Value(authenticatedUsernameKey).(string)
	if !ok || username == "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return "", false
	}
	return username, true
}

func formID(r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(r.Form.Get(key), 10, 64)
	return id, err == nil && id > 0
}

func ownsPlaylist(ctx context.Context, id int64, username string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM playlists WHERE id = ? AND ownername = ?`, id, username).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// songIDForFile turns the "file" string used by the page into a songs.id.
func songIDForFile(ctx context.Context, rel string) (int64, error) {
	fullPath, _, err := safeMediaPath(rel)
	if err != nil {
		return 0, err
	}
	var songID int64
	err = db.QueryRowContext(ctx, `
		SELECT song_id FROM media_locations
		WHERE file_path = ? AND is_available = 1
	`, filepath.Clean(fullPath)).Scan(&songID)
	return songID, err
}

// ---------------------------------------------------------------- handlers

func playlistsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	username, ok := r.Context().Value(authenticatedUsernameKey).(string)
	if !ok || username == "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}

	// Song details (title, RelPath) come from the catalog, indexed by id.
	// NOTE: this finishes before the next query starts. The DB is limited to ONE
	// connection (SetMaxOpenConns(1)), so never run a query while another
	// rows cursor is still open, or the request will hang.
	catalogSongs, err := loadCatalogSongs()
	if err != nil {
		http.Error(w, "Could not read catalog", http.StatusInternalServerError)
		return
	}
	byID := make(map[int64]Song, len(catalogSongs))
	for _, s := range catalogSongs {
		byID[s.ID] = s
	}

	// One query: playlists LEFT JOIN songs, so empty playlists still appear.
	rows, err := db.QueryContext(r.Context(), `
		SELECT p.id, p.name, ps.song_id
		FROM playlists p
		LEFT JOIN playlist_songs ps ON ps.playlist_id = p.id
		WHERE p.ownername = ?
		ORDER BY p.created_at, p.id, ps.position
	`, username)
	if err != nil {
		http.Error(w, "Could not read playlists", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	playlists := []Playlist{} // [] not null in the JSON
	position := map[int64]int{}
	for rows.Next() {
		var id int64
		var name string
		var songID sql.NullInt64
		if err := rows.Scan(&id, &name, &songID); err != nil {
			http.Error(w, "Could not read playlists", http.StatusInternalServerError)
			return
		}
		i, seen := position[id]
		if !seen {
			playlists = append(playlists, Playlist{ID: id, Name: name, Songs: []PlaylistSong{}})
			i = len(playlists) - 1
			position[id] = i
		}
		if songID.Valid {
			if s, ok := byID[songID.Int64]; ok { // skips songs whose file is gone
				playlists[i].Songs = append(playlists[i].Songs,
					PlaylistSong{ID: s.ID, File: s.RelPath, Title: s.Title})
			}
		}
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "Could not read playlists", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, playlists)
}

func playlistCreateHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := postUser(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	if name == "" || utf8.RuneCountInString(name) > 100 {
		http.Error(w, "Playlist name must be 1-100 characters", http.StatusBadRequest)
		return
	}
	result, err := db.ExecContext(r.Context(),
		`INSERT INTO playlists (name, ownername) VALUES (?, ?)`, name, username)
	if err != nil {
		fmt.Printf("Could not create playlist for %q: %v\n", username, err)
		http.Error(w, "Could not create playlist", http.StatusInternalServerError)
		return
	}
	id, _ := result.LastInsertId()
	writeJSON(w, http.StatusCreated, Playlist{ID: id, Name: name, Songs: []PlaylistSong{}})
}

func playlistDeleteHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := postUser(w, r)
	if !ok {
		return
	}
	id, ok := formID(r, "playlist_id")
	if !ok {
		http.Error(w, "Bad playlist id", http.StatusBadRequest)
		return
	}
	owns, err := ownsPlaylist(r.Context(), id, username)
	if err != nil {
		http.Error(w, "Could not delete playlist", http.StatusInternalServerError)
		return
	}
	if !owns {
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}

	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "Could not delete playlist", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM playlist_songs WHERE playlist_id = ?`, id); err == nil {
		_, err = tx.Exec(`DELETE FROM playlists WHERE id = ? AND ownername = ?`, id, username)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fmt.Printf("Could not delete playlist %d: %v\n", id, err)
		http.Error(w, "Could not delete playlist", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func playlistAddSongHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := postUser(w, r)
	if !ok {
		return
	}
	playlistID, ok := formID(r, "playlist_id")
	if !ok {
		http.Error(w, "Bad playlist id", http.StatusBadRequest)
		return
	}
	owns, err := ownsPlaylist(r.Context(), playlistID, username)
	if err != nil {
		http.Error(w, "Could not add song", http.StatusInternalServerError)
		return
	}
	if !owns {
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}
	songID, err := songIDForFile(r.Context(), r.Form.Get("file"))
	if err != nil {
		http.Error(w, "Song is not in the catalog", http.StatusNotFound)
		return
	}

	// position = last position + 1; adding a song that is already there does nothing
	_, err = db.ExecContext(r.Context(), `
		INSERT INTO playlist_songs (playlist_id, song_id, position)
		VALUES (?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM playlist_songs WHERE playlist_id = ?))
		ON CONFLICT(playlist_id, song_id) DO NOTHING
	`, playlistID, songID, playlistID)
	if err == nil {
		_, err = db.ExecContext(r.Context(),
			`UPDATE playlists SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, playlistID)
	}
	if err != nil {
		fmt.Printf("Could not add song %d to playlist %d: %v\n", songID, playlistID, err)
		http.Error(w, "Could not add song", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func playlistRemoveSongHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := postUser(w, r)
	if !ok {
		return
	}
	playlistID, ok1 := formID(r, "playlist_id")
	songID, ok2 := formID(r, "song_id")
	if !ok1 || !ok2 {
		http.Error(w, "Bad id", http.StatusBadRequest)
		return
	}
	// the subquery makes sure the playlist belongs to this user
	_, err := db.ExecContext(r.Context(), `
		DELETE FROM playlist_songs
		WHERE playlist_id = ? AND song_id = ?
		  AND playlist_id IN (SELECT id FROM playlists WHERE ownername = ?)
	`, playlistID, songID, username)
	if err != nil {
		fmt.Printf("Could not remove song %d from playlist %d: %v\n", songID, playlistID, err)
		http.Error(w, "Could not remove song", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
