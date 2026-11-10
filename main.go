package main

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const musicRoot = "D:/gaane_novideo"
const port = ":8080"

type Song struct {
	Title    string
	FullPath string
	RelPath  string
}

// PageData holds data passed into the HTML template
type PageData struct {
	MusicRoot string
	Songs     []Song
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
				if !subEntry.IsDir() && isAudioFile(subEntry.Name()) {
					subFullPath := filepath.Join(fullPath, subEntry.Name())
					rel, _ := filepath.Rel(musicRoot, subFullPath)
					songs = append(songs, Song{
						Title:    subEntry.Name(),
						FullPath: subFullPath,
						RelPath:  filepath.ToSlash(rel),
					})
				}
			}
		} else if isAudioFile(entry.Name()) {
			rel, _ := filepath.Rel(musicRoot, fullPath)
			songs = append(songs, Song{
				Title:    entry.Name(),
				FullPath: fullPath,
				RelPath:  filepath.ToSlash(rel),
			})
		}
	}

	return songs, nil
}

func isAudioFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return ext == ".mp3" || ext == ".flac" || ext == ".wav" || ext == ".m4a"
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	songs, err := scanMusicFolder(musicRoot)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error scanning music folder: %v", err), http.StatusInternalServerError)
		return
	}

	// Parse the external HTML file template
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("Template parsing error: %v", err), http.StatusInternalServerError)
		return
	}

	data := PageData{
		MusicRoot: musicRoot,
		Songs:     songs,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, data)
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("file")
	if relPath == "" {
		http.Error(w, "Missing file query parameter", http.StatusBadRequest)
		return
	}

	safePath := filepath.Join(musicRoot, filepath.Clean("/"+relPath))
	
	file, err := os.Open(safePath)
	if err != nil {
		http.Error(w, "Song file not found", http.StatusNotFound)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	buf := make([]byte, 4096)
	for {
		n, err := file.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return
			}
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}
}

func main() {
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/stream", streamHandler)

	fmt.Printf("[*] Clean MVC-style music server started. Open http://localhost%s in your browser\n", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		fmt.Printf("Server failed: %v\n", err)
	}
}