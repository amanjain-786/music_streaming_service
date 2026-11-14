# Private Playlists

## Goal

Add private, user-owned playlists to the music and video library. Playlists contain catalog song IDs in a defined order. A song may appear in multiple playlists, but only once in each playlist.

## Data model

- `playlists`: playlist ID, owner username, name, and creation time.
- `playlist_items`: playlist ID, song ID, and position. Enforce unique `(playlist_id, song_id)` and `(playlist_id, position)` constraints.
- Extend `user_playback_state` with a nullable playlist ID so saved playback can resume within the playlist that started it.

Playlist ownership is determined from the authenticated session cookie on the server. Never accept an owner username from the client.

## Behavior

- Users can create, view, rename, and delete only their own playlists.
- Users can add catalog songs to multiple playlists and remove songs from their own playlists.
- Preserve playlist item order for display and playback.
- When a song is played from a playlist, next/previous and autoplay follow that playlist’s order. Library playback continues to follow the current filtered library order.
- Resume state stores the song ID, position, and optional playlist ID.

## Interface

- Add a left sidebar that is open by default on desktop.
- The sidebar lists the signed-in user’s playlists and provides playlist creation and selection.
- Show the signed-in username and logout control in the sidebar.
- On narrow screens, collapse the sidebar and provide a button to open and close it.
- Selecting a playlist displays its ordered songs and provides controls to add or remove songs.

## API and security

- Protect playlist routes with the existing authentication middleware.
- Derive the playlist owner from the authenticated session.
- Verify ownership on every playlist read or mutation.
- Use catalog song IDs for playlist items and playback references.

## Verification

- Test ownership isolation between users.
- Test that duplicate songs cannot be added to one playlist but can be added to different playlists.
- Test ordering, removal, and deletion behavior.
- Test that playlist playback navigation follows playlist order.
- Test that saved playback resumes with the correct song, position, and playlist context.
- Check responsive sidebar behavior on desktop and narrow screens.
