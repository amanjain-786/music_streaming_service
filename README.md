# Music Streamer

A local Go music and video library backed by SQLite. The server reads its library from the persistent catalog; media folders are scanned only when the catalog command is run.

## Requirements

- Go 1.27 or newer
- Windows paths are supported by the current configuration; Go's path handling also supports other operating systems
- Media folders readable by the account running the commands

## Configuration

Copy `config.example.json` to `config.json`, then replace the placeholder media folders with your own paths:

```json
{
  "media_roots": ["C:/Path/To/Music", "C:/Path/To/Videos"],
  "database_path": "plays.db",
  "credentials_file": "users.txt",
  "listen_address": ":8080"
}
```

`config.json` is ignored by Git, while `config.example.json` contains only placeholders and is safe to commit. Paths may be absolute or relative to `config.json`. The config is shared by the server and catalog scanner. The database and credentials should be kept private. `users.txt` is ignored by Git and must contain one username and password per line, separated by whitespace or a tab.

## Commands

Run commands from this project directory.

Build the catalog before the first server start:

```powershell
go run . scan
```

Start the web server:

```powershell
go run . server
```

The server is also the default command:

```powershell
go run .
```

Use a different config file with either command:

```powershell
go run . scan --config path\to\config.json
go run . server --config path\to\config.json
```

Reset play counts, saved playback positions, and remove old path-based play tables while preserving the catalog:

```powershell
go run . reset-play-data --yes
```

The scan command is safe to run repeatedly and can be scheduled independently. It recursively discovers supported audio and video files. It hashes new files and files whose size or modification time changed; unchanged files are not reread. Files with identical SHA-256 content share a row in `songs`, while each distinct path has its own unique row in `media_locations`. Each scan prints duplicate file paths with the first matching catalog path and content hash. A completed scan marks missing paths unavailable instead of deleting them. If a configured root cannot be scanned, that root's catalog state is left unchanged and the command reports an error.

The server does not scan folders at startup or for each page request. Its library view contains songs with at least one available catalog location. Run `go run . scan` after adding, removing, moving, or changing files.

## Catalog schema

- `songs.id` is the stable internal song ID.
- `songs.content_hash` is a unique SHA-256 hash of the file bytes. Re-encoded copies are different catalog songs even if they represent the same recording.
- `media_locations.file_path` is unique. Multiple locations with identical contents point to the same song ID.
- Unavailable locations and their song IDs are retained so future playlists and queues can continue to reference stable IDs.

Play counts and resume positions are keyed by stable song ID: `song_play_counts` stores global totals, `user_song_plays` stores each user's totals, and `user_playback_state` stores each user's last song and position. The reset command clears these records and drops the old path-based `plays`, `user_plays`, and `playback_state` tables. Normal server and scan startup do not migrate or clear play data. New timestamps use SQLite's `CURRENT_TIMESTAMP` format.

## Development checks

```powershell
go test ./...
go build ./...
```
