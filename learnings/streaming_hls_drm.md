BRO. 😂 Let's turn this into something you can confidently explain in a software engineering interview, not just something you know how to run.

We're going to approach this like an actual system-design discussion.

Your project is a self-hosted, authenticated music streaming platform built with Go. The interesting engineering part isn't just playing music. It's how you deliver media efficiently, control access, support seeking, and protect the original files.

There are six major topics you should understand:

1. Why direct MP3 serving has limitations.
2. What streaming actually means.
3. What HLS is and how it works.
4. How authentication and authorization work with HLS.
5. What DRM is, and how it differs from HLS encryption.
6. How to explain and defend these decisions in an interview.

One important distinction before we start: your current Go application already supports HTTP range-based media delivery through `http.ServeContent()`. It is not yet an HLS implementation. We'll use your existing implementation as the starting point.

# 1. First, understand what you're actually building

Imagine your PC has this file:

```
D:/gaane/song.mp3
```

Your friend opens your music website and clicks Play.

Your Go server reads that file and sends the audio to your friend's browser.

In your current implementation:

```
w.Header().Set("Content-Type", contentType(full))
http.ServeContent(w, r, stat.Name(), stat.ModTime(), file)
```

This is from your `streamHandler`.&#x20;

Pasted text(1).txt



The browser receives audio data and plays it.

So far, straightforward.

But let's ask an important engineering question:

Why not just give the browser the MP3 file directly? Why introduce more complicated streaming technologies?

# 2. Why direct MP3 serving has limitations

First, understand that direct MP3 serving is not necessarily bad.

In fact, for a small application, it is a perfectly reasonable approach.

However, it has several limitations as your application grows.

## Problem A: The original file is directly accessible

Suppose your server exposes:

```
GET /stream?file=Song.mp3
```

A user can open that URL in a browser or use PowerShell:

```
Invoke-WebRequest `
  "https://example.com/stream?file=Song.mp3" `
  -OutFile "Song.mp3"
```

If the user is authorized and the endpoint allows it, the entire original file can be retrieved.

Why?

Because your server is effectively exposing the original media resource.

Your authentication middleware checks whether someone has logged in, but it doesn't distinguish between:

- A request to play a song.
- A request to download the entire song.

Both are requests to access the same resource.

This is an important distinction:

Authentication tells us who the user is. Authorization tells us what that user is allowed to do.

You currently have authentication, but you don't have a separate playback-versus-download permission model.

## Problem B: A single file is one complete media resource

Suppose you have a 100 MB audio file.

The browser requests it and starts playing while the data arrives.

This is commonly called progressive downloading or progressive playback.

The browser doesn't necessarily wait for all 100 MB before playing, but the resource itself is still the original complete file.

The HTTP Range mechanism makes this better.

For example:

```
GET /stream?file=Song.mp3
Range: bytes=1000000-2000000
```

The server can return:

```
HTTP/1.1 206 Partial Content
Content-Range: bytes 1000000-2000000/50000000
```

This means the browser is requesting a particular byte range rather than the entire file.

Your existing `http.ServeContent()` supports this automatically.

## Problem C: Limited control over playback quality

Imagine your original audio file is encoded at 320 kbps.

One friend has a slow internet connection.

Another friend has a fast connection.

With direct MP3 serving, both generally receive the same encoded file.

You can create multiple MP3 versions and choose between them yourself, but you don't get HLS's standardized adaptive-bitrate delivery mechanism automatically.

## Problem D: The original format is exposed

With direct MP3 delivery, users receive the actual MP3 representation.

They can save it without needing to reconstruct anything.

For your application, this is probably the biggest issue because you specifically want people to listen without casually obtaining your original music files.

## So is direct MP3 bad?

No.

| Scenario                           | Direct MP3                      |
| ---------------------------------- | ------------------------------- |
| Small personal music website       | Excellent                       |
| Few users                          | Excellent                       |
| Simple implementation              | Excellent                       |
| Seeking within a song              | Supported with HTTP Range       |
| Multiple quality levels            | Requires additional logic/files |
| Hiding original files              | Not sufficient                  |
| Fine-grained segment authorization | Not built in                    |

Interview answer:

"I initially used progressive HTTP delivery with Range support because it was simple and efficient for my initial use case. However, it exposed the original media resource directly and offered limited adaptive-bitrate control. I explored HLS to introduce segmented delivery, per-segment authorization, and multiple quality levels."

That's a good engineering explanation. Notice that you're not claiming MP3 streaming is inherently inefficient.

# 3. What is streaming?

Streaming means consuming media progressively as it is delivered, instead of requiring the entire media file to be downloaded before playback can begin.

Consider a 10-minute song.

At 192 kbps, its approximate size is:

\\[ \frac{192,000 \times 600}{8} = 14,400,000 \text{ bytes} \\]

That's approximately 14.4 MB.

There are two concepts worth distinguishing.

### A. Progressive HTTP streaming

This is what your current Go application uses.

Progressive delivery

One original MP3 file

Browser receives bytes progressively

Browser starts playback

The browser can request additional byte ranges when the listener seeks.

### B. Segmented streaming

Instead of serving one large media resource, you divide the content into smaller playable segments.

Segmented delivery

Original audio

0–6 sec

6–12 sec

12–18 sec

18–24 sec

24–30 sec

30–36 sec

Browser requests segments as needed

This is the foundation of HLS.

# 4. What exactly is HLS?

HLS stands for HTTP Live Streaming.

It was originally developed by Apple and is now widely used for delivering audio and video over HTTP.

The idea is simple:

Instead of sending the complete media file as one resource, the server divides it into smaller segments and provides a playlist describing those segments.

HLS usually involves two types of files:

1. A playlist (`.m3u8`).
2. Media segments (`.ts` or `.m4s`, depending on the packaging).

Let's see a real example.

## A. The playlist

Suppose you have a song called `song.mp3`.

Your media packaging process generates:

```
song/
    master.m3u8
    192k/
        playlist.m3u8
        segment000.ts
        segment001.ts
        segment002.ts
        segment003.ts
```

The `playlist.m3u8` might look like:

```
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXTINF:6.0,
segment000.ts
#EXTINF:6.0,
segment001.ts
#EXTINF:6.0,
segment002.ts
#EXTINF:6.0,
segment003.ts
#EXT-X-ENDLIST
```

Let's understand this.

| Tag                     | Meaning                                       |
| ----------------------- | --------------------------------------------- |
| `#EXTM3U`               | Identifies an M3U playlist                    |
| `#EXT-X-VERSION`        | Playlist protocol version                     |
| `#EXT-X-TARGETDURATION` | Maximum segment duration, rounded up          |
| `#EXTINF`               | Duration of the following segment             |
| `#EXT-X-ENDLIST`        | Indicates that no more segments will be added |

Here, each segment is approximately six seconds long.

The browser doesn't need to receive the entire song at once.

It can request the playlist, then retrieve the segments as playback progresses.

## B. How playback happens

Let's imagine your friend clicks Play.

1. Browser requests the playlist

   `GET /hls/song/playlist.m3u8`
2. Go server authenticates the request

   It checks the session and whether the user can access this song.
3. Server returns the playlist

   The browser discovers the available segments.
4. Browser requests segments

   `GET /hls/song/segment000.ts`
5. Go server checks authorization again

   It validates access to the requested segment.
6. Browser plays the received audio

   It continues requesting subsequent segments.

An important detail: authorization must happen on every segment request, not just when the playlist is requested.

Otherwise, someone could obtain a playlist once and then access its segments without further authorization.

## C. What makes HLS particularly useful?

One of its biggest advantages is adaptive bitrate streaming.

Imagine you generate three versions of the same song:

| Version | Bitrate  | Approximate bandwidth |
| ------- | -------- | --------------------- |
| Low     | 96 kbps  | 12 KB/s               |
| Medium  | 192 kbps | 24 KB/s               |
| High    | 320 kbps | 40 KB/s               |

You can create a master playlist:

```
#EXTM3U

#EXT-X-STREAM-INF:BANDWIDTH=110000
96k/playlist.m3u8

#EXT-X-STREAM-INF:BANDWIDTH=220000
192k/playlist.m3u8

#EXT-X-STREAM-INF:BANDWIDTH=350000
320k/playlist.m3u8
```

The player can select a suitable rendition based on available bandwidth and playback conditions.

For example:

- Poor connection: 96 kbps.
- Normal connection: 192 kbps.
- Fast connection: 320 kbps.

This is called Adaptive Bitrate Streaming (ABR).

One nuance: HLS doesn't automatically make every browser switch quality perfectly. The player needs to support adaptive selection, and the media renditions need compatible segment boundaries and timing.

For your music application, you could also let users manually choose quality.

## D. How does seeking work with HLS?

Suppose a song is five minutes long and your friend jumps to 3:30.

With direct MP3 delivery, the browser requests the corresponding byte range.

With HLS, the player calculates which segment corresponds to that playback position and requests the relevant segment.

If each segment is six seconds:

\\[ 210 / 6 = 35 \\]

So the player needs to locate approximately segment 35.

It can then fetch that segment and continue playback.

This is one reason segment duration matters.

Shorter segments can improve seeking granularity and reduce the amount of media fetched unnecessarily, but they also create more HTTP requests and overhead.

# 5. Now the important part: HLS authentication

This is probably the most interesting backend engineering component for your resume.

Let's say your friend logs into your application.

Your current Go server issues a session cookie:

```
Set-Cookie: music_session=abc123;
HttpOnly;
SameSite=Strict;
```

Your browser automatically sends that cookie on subsequent requests to your application.

Your existing Go middleware checks that cookie before allowing access to protected routes.

You already have this foundation.

However, HLS introduces multiple requests for one song.

For example:

```
GET /hls/song/playlist.m3u8
GET /hls/song/segment000.ts
GET /hls/song/segment001.ts
GET /hls/song/segment002.ts
GET /hls/song/segment003.ts
```

Every request needs authorization.

## A. Why protecting only the playlist is insufficient

Imagine this implementation:

```
GET /hls/song/playlist.m3u8
```

The server checks the user's login and returns:

```
#EXTM3U
#EXTINF:6,
segment000.ts
#EXTINF:6,
segment001.ts
```

But the segment endpoints are public.

What happens?

A person could obtain the playlist and access:

```
/hls/song/segment000.ts
```

without being authenticated.

You've protected the playlist, but not the actual media.

This is a classic authorization mistake.

The playlist is not the media. The segments are the media.

Both must be protected.

## B. The simplest solution: cookie-based authentication

This is probably what I'd implement first in your Go application.

Your browser already has a session cookie after login.

When the HLS player requests:

```
/hls/song/playlist.m3u8
```

it sends the cookie.

When it requests:

```
/hls/song/segment000.ts
```

it sends the cookie again, provided the request is same-origin and the cookie's scope and security settings permit it.

Your Go middleware validates the cookie on every request.

The architecture:

User logs in

Go creates a session

Browser stores HttpOnly cookie

Every HLS request

Playlist

Segment

Go auth middleware

Authorized → serve media

Unauthorized → 401/redirect

Advantages:

- Simple to implement.
- Reuses your existing session system.
- No tokens need to be embedded in the playlist.
- Logging out can invalidate the session immediately.
- Works well when your frontend and backend share an origin.

For your current setup, this is sufficient to get started.

One caveat: if you use an HLS player such as hls.js, make sure its requests include credentials when needed. Native browser HLS behavior and JavaScript HLS player behavior aren't identical.

## C. The next level: signed URLs

Now imagine someone copies this:

```
https://example.com/hls/song/segment000.ts
```

Even if they share that URL with another person, the other person won't have the correct session cookie.

But we can introduce another layer.

A signed URL.

For example:

```
/hls/song/segment000.ts?token=abc123
```

The token contains or represents information such as:

- Which song can be accessed.
- Which user it belongs to.
- When the token expires.
- Which media resource it permits.

A typical implementation uses an HMAC signature.

HMAC is a message authentication mechanism based on a secret key.

For example, conceptually:

```
payload = userID + songID + expiry

signature = HMAC-SHA256(secret, payload)
```

The server verifies the signature when someone requests the segment.

If the signature is invalid or expired, the server rejects the request.

A simplified Go example:

```
func signToken(payload string, secret []byte) string {
    mac := hmac.New(sha256.New, secret)
    mac.Write([]byte(payload))

    return hex.EncodeToString(mac.Sum(nil))
}
```

The verification process recalculates the HMAC and compares the result using a constant-time comparison.

Important: a signed token is not necessarily encrypted. The user may be able to read its payload. The signature prevents them from modifying it undetected.

### Why use signed URLs?

Imagine a user requests a song.

Your Go backend generates a playlist with signed segment URLs:

```
#EXTM3U
#EXTINF:6,
segment000.ts?token=abc
#EXTINF:6,
segment001.ts?token=def
```

Each URL can have a short expiry.

This makes copied links less useful after expiration.

However, signed URLs have trade-offs:

- Someone can copy and reuse them while they remain valid.
- Every segment URL must be signed or covered by a valid authorization scheme.
- Tokens in query strings can leak through logs and analytics.
- If tokens are reusable, they don't prove that the original user is making the request.

A stronger design binds access to a user session as well as the token.

My recommendation: use your existing HttpOnly session cookie first. Add signed URLs if you have a specific reason to support stateless or separately authorized media access.

# 6. HLS encryption: where things get interesting

You might now be thinking:

"Okay, I've divided my song into segments and protected every segment with authentication. Can I encrypt those segments too?"

Yes.

HLS supports encryption.

One common mechanism is AES-128 encryption.

## A. How AES-128 HLS encryption works

Imagine your original song is converted into six-second segments.

Instead of storing ordinary playable segments, your packaging process encrypts them.

Conceptually:

```
segment000.ts → encrypted segment
segment001.ts → encrypted segment
segment002.ts → encrypted segment
```

The playlist includes an encryption instruction:

```
#EXTM3U
#EXT-X-VERSION:3

#EXT-X-KEY:METHOD=AES-128,URI="/hls/keys/song.key"

#EXTINF:6,
segment000.ts

#EXTINF:6,
segment001.ts

#EXTINF:6,
segment002.ts
```

The player sees the `EXT-X-KEY` instruction and requests:

```
GET /hls/keys/song.key
```

Your server authenticates the request and returns the decryption key.

The player uses that key to decrypt the segments and play the audio.

## B. Why encrypt if the player needs the key?

Good question.

Encryption provides protection for the stored and transmitted media segments.

For example, if somebody gets access to your media directory, they cannot simply play the encrypted segments.

It also means that possessing a segment URL alone isn't enough to play the segment.

But there's a weakness:

The player must obtain the key to play the song.

If a user can access the key and all the segments, they can potentially decrypt and reconstruct the song.

So AES-128 HLS encryption is useful, but it is not equivalent to DRM.

This distinction is extremely important in interviews.

| Technology     | What it protects                                        |
| -------------- | ------------------------------------------------------- |
| HTTPS          | Data travelling between client and server               |
| HLS            | Media delivery through playlists and segments           |
| HLS AES-128    | Media segments using encryption                         |
| Authentication | User identity                                           |
| Authorization  | Which media a user can access                           |
| Signed URLs    | Authorization of specific requests for a limited time   |
| DRM            | Controlled decryption and playback through a DRM system |

These technologies solve different problems. They can be combined.

# 7. What is DRM, then?

DRM stands for Digital Rights Management.

Its purpose is to control how digital media can be accessed and consumed.

Unlike basic HLS encryption, commercial DRM systems introduce a dedicated content decryption component and a license mechanism.

Three well-known technologies:

[How does Google's Widevine DRM protect your Videos?](https://images.openai.com/static-rsc-4/EsIZiQ6B1PNMgKwHXjfcphw-97iJX9qGCwySyHPjOnUR_a_whS5a7AHAexQS3ucgnX-wnAywDaVi5_9rmZ3tfWt1yYhIdSLC86fALqCOht4jMmYCdDQy2kcrKlwY6Nw6Tib03ZbvOZuneFsB3kqr9Qnyim4DGmN1PMBKqBcDpMQ?purpose=inline)

Google Widevine

Common in Chrome and Android streaming environments.

[PlayReady Ecosystem - PlayReady | Microsoft Learn](https://images.openai.com/static-rsc-4/Iy9_-WSjkZ2y9kTt5LexfSChHaH97Nwhm8WqflMgh0e4ieaRmz9yCGMvhahe0aEtZuUOZwbmAnxCXZnygvNnnNjSPDikhXA7thGpkTFsjPjiSoSm4Zp1VU3V5BgYgZiuFu6H5uiiuuKUFAhkcceDCBPpmv9xA7QGn2xN0NIax50?purpose=inline)

Microsoft PlayReady

Common in Microsoft and supported television environments.

[Multimedia Security Topics | meteor Tech Blog](https://images.openai.com/static-rsc-4/IS-2TxuHI3mWGYMNcoJQVfVe1n9un_1br4BiEQw4VX0PZxw4rxoUWBwW6GYNdvmhv5PYcFhdGnhHtTZQdkkC7M50IPHb39RiFIpStrvqyR_70MqmBSD36Jn3PsHtKYhdDROi4apH8OUt6VUm62ooOFzQnMmRk2egDp-HZelcA38?purpose=inline)

Apple FairPlay Streaming

Apple's DRM system for supported streaming environments.

## A. How DRM works

Suppose your friend wants to play an encrypted song.

The process is roughly:

1. The browser downloads encrypted media.
2. The browser's Encrypted Media Extensions (EME) API communicates with a Content Decryption Module (CDM).
3. The CDM generates a license challenge.
4. The license server checks the user's authorization.
5. The license server issues an appropriate license.
6. The CDM uses the license to decrypt the media.
7. The browser plays the decrypted audio.

Encrypted media segments

Browser / EME

CDM — Content Decryption Module

Authorized decryption and playback

License server

Validates authorization and issues licenses to the DRM system.

The CDM may use hardware-backed security and protected media paths, depending on the device and DRM security level.

## B. Why DRM is not 100% protection

Think about it this way:

You can encrypt a song perfectly.

You can prevent users from extracting the decryption key through ordinary browser JavaScript.

You can use hardware-backed security.

But the user still needs to hear the song.

At some point, audio must be delivered to an output device.

Someone can record that output using another device.

This is known as the analog hole.

DRM also depends on the security of the operating system, browser, hardware and CDM implementation. Vulnerabilities or compromised devices can undermine those protections.

Therefore:

- HLS encryption protects the media segments.
- DRM protects the decryption process and playback environment.
- Neither guarantees that the listener cannot make a recording.

## C. Do you need DRM?

For your project, honestly, no.

DRM introduces:

- License-server infrastructure.
- DRM provider requirements.
- Device and browser compatibility problems.
- More complex content packaging.
- Additional debugging and operational costs.

You're building a private music server for approximately five people.

Implementing DRM would be disproportionate to your requirements.

A good engineering decision is knowing when not to introduce complexity.

# 8. How I'd architect your actual Go application

Now let's bring everything together.

This is the architecture I'd recommend you eventually implement.

Cloudflare Tunnel

External HTTPS + email OTP gate

Go HTTP server

Authentication, sessions, authorization, rate limiting

Playlist handler

Returns authorized M3U8 playlists

Segment handler

Checks access before serving segments

HLS media storage

Playlists and audio segments generated by FFmpeg

Original music library

Private folders on your PC; never directly exposed

## A. Suggested Go routes

You currently have:

```
/login
/logout
/stream
/played
/
```

You could eventually add:

```
/hls/:songID/master.m3u8
/hls/:songID/:quality/playlist.m3u8
/hls/:songID/:quality/:segment
```

For example:

```
/hls/123/master.m3u8

/hls/123/192k/playlist.m3u8

/hls/123/192k/segment000.ts
/hls/123/192k/segment001.ts
/hls/123/192k/segment002.ts
```

Each route should validate the user session and check whether that user is permitted to play the requested song.

Never construct a filesystem path directly from an arbitrary URL parameter. Resolve song IDs against your media database or a trusted internal mapping.

## B. How FFmpeg fits in

Useful tool to learn

FFmpeg is the tool I'd use for packaging your original music into HLS.

For example, a simplified command:

```
ffmpeg -i "song.mp3" `
  -vn `
  -c:a aac `
  -b:a 192k `
  -f hls `
  -hls_time 6 `
  -hls_playlist_type vod `
  -hls_segment_filename "output/segment%03d.ts" `
  "output/playlist.m3u8"
```

What happens here?

| Argument                 | Purpose                                  |
| ------------------------ | ---------------------------------------- |
| `-i`                     | Input audio                              |
| `-vn`                    | Ignore video                             |
| `-c:a aac`               | Encode audio using AAC                   |
| `-b:a 192k`              | Set audio bitrate                        |
| `-f hls`                 | Produce HLS output                       |
| `-hls_time 6`            | Target approximately six-second segments |
| `-hls_playlist_type vod` | Mark it as video-on-demand content       |
| `-hls_segment_filename`  | Define segment filenames                 |

A practical implementation should also account for codec compatibility and segment boundaries. For a first prototype, one quality level is enough.

I would pre-generate and cache HLS output rather than re-encoding every song every time somebody presses Play.

That saves CPU and makes simultaneous playback much easier.

## C. Important implementation detail: browser playback

HLS isn't universally supported natively in every desktop browser.

For example:

- Safari has native HLS support.
- Chrome and Firefox commonly use a JavaScript HLS player such as hls.js with Media Source Extensions.

Your frontend could use:

```
const video = document.querySelector("audio");
const source = "/hls/123/master.m3u8";

if (video.canPlayType("application/vnd.apple.mpegurl")) {
    video.src = source;
} else if (Hls.isSupported()) {
    const hls = new Hls();

    hls.loadSource(source);
    hls.attachMedia(video);
}
```

For an actual authenticated implementation, configure the HLS player's requests to include credentials where required.

For same-origin requests, this is generally straightforward.

# 9. Interview questions you should be ready for

This is where we turn knowledge into answers.

## Q1. Why did you choose HLS instead of serving MP3 files directly?

Strong answer:

"I initially implemented progressive HTTP delivery using Go's `http.ServeContent`, which supports Range requests and seeking. However, direct delivery exposed the original media resource and didn't provide a standardized adaptive-bitrate architecture. HLS allows me to package media into segments, provide multiple quality levels, and authorize individual playlist and segment requests."

## Q2. Is HLS itself secure?

Strong answer:

"No. HLS is a media delivery protocol, not an authentication or security mechanism. Playlists and segments can be publicly accessible unless the application enforces authorization. I protect both playlist and segment endpoints using authenticated sessions and can introduce expiring signed URLs for additional control."

## Q3. What is the difference between authentication and authorization?

Strong answer:

"Authentication establishes the identity of a user. Authorization determines whether that user is allowed to perform a specific action or access a particular resource. In my application, a valid session establishes identity, while checking whether that user can play a particular song is authorization."

## Q4. Why not just encrypt all HLS segments?

Strong answer:

"Encryption protects the media segments, but the player needs the decryption key. If the key is accessible to the user, the media can potentially be reconstructed. Commercial DRM goes further by managing licenses and controlling decryption through a protected playback environment."

## Q5. What is the difference between HLS and DRM?

Strong answer:

"HLS defines how media is packaged and delivered through playlists and segments. DRM controls access to decryption keys and the playback environment. They are complementary technologies rather than alternatives."

## Q6. Can HLS prevent downloading?

Strong answer:

"No. HLS makes direct retrieval of the original file less straightforward, but authorized users still receive the media segments. They can potentially save and reconstruct them. My objective is to prevent casual extraction and unauthorized access, not claim that copying is physically impossible."

## Q7. Why use short-lived signed URLs?

Strong answer:

"Signed URLs provide time-limited authorization for specific resources. A signature allows the server to verify that the URL hasn't been tampered with, without needing to maintain a separate database record for every URL. However, a signed URL can be replayed until it expires, so I combine it with user session checks when stronger access control is needed."

## Q8. What happens if a user logs out while a song is playing?

Strong answer:

"The server invalidates their session. Subsequent playlist or segment requests fail authorization. Already buffered media may continue playing briefly, because revoking access cannot erase data already delivered to the client."

That's a particularly good detail to mention in an interview.

## Q9. Why did you use Go?

Strong answer:

"Go provides a lightweight HTTP server, efficient concurrent request handling, straightforward filesystem access, and a simple deployment model. It suits a self-hosted application where multiple users can stream media concurrently without requiring a large runtime."

## Q10. What are the bottlenecks?

Strong answer:

"The primary constraints are home upload bandwidth, disk throughput, transcoding CPU usage, and concurrent requests. Pre-generating HLS segments reduces runtime transcoding costs. I would monitor bandwidth, request latency, active sessions, and segment delivery errors."

# 10. How to describe this project on your resume

Here's how I'd eventually describe it, assuming you implement the HLS features we've discussed.

## Self-Hosted Music Streaming Platform

GO · HTTP · HLS · FFmpeg · SQLITE · CLOUDFLARE TUNNEL

- Developed a self-hosted music streaming application using Go, with authenticated user sessions and per-user playback statistics.
- Implemented HLS-based segmented media delivery using FFmpeg, enabling adaptive bitrate playback and seeking.
- Designed per-request authorization for playlists and media segments using session-based authentication and time-limited access tokens.
- Integrated Cloudflare Tunnel for secure external access without exposing the home network's public IP directly to application users.
- Used SQLite for persistent playback history and user-specific listening statistics.

Important: Don't put the HLS, adaptive bitrate, or signed URL claims on your resume until you've actually implemented and tested them. Your current code already supports progressive delivery, Range requests, login sessions, rate limiting and SQLite-based playback tracking.

# 11. Your learning roadmap

I wouldn't implement everything simultaneously. I'd build this in stages.

Implementation checklist

0/8 completed

1\. Understand HTTP streaming and Range requests

Inspect 200 vs 206 responses and test seeking in your existing application.

2\. Learn FFmpeg and HLS packaging

Convert one MP3 into a playlist and segments.

3\. Build the Go playlist handler

Serve M3U8 playlists with proper content types.

4\. Build the authenticated segment handler

Protect every segment request using your existing sessions.

5\. Integrate hls.js

Make playback work in Chrome and Firefox, with Safari native support.

6\. Add multiple bitrates

Generate multiple renditions and a master playlist.

7\. Add signed URLs and expiry

Test expired, modified and replayed URLs.

8\. Test concurrent users

Measure bandwidth, latency, seeking and playback failures.

# 12. Let's test whether you actually understand it

I want you to be able to answer these without memorizing paragraphs.

Imagine you're in an interview and I ask:

Your application uses HLS. A user logs in, downloads the `.m3u8` playlist, and sends it to their friend. Your friend doesn't have an account. Can they play the song? Why or why not?

Think about:

- What does the playlist contain?
- Where are the actual audio bytes?
- Does protecting the playlist automatically protect the segments?
- What does your Go server check when a segment is requested?

This is exactly the kind of question that separates someone who has merely integrated a library from someone who understands the architecture.

My suggestion: implement one song end-to-end with HLS before touching the rest of your music application. Once you understand playlist generation, authorization, segment delivery and browser playback, scaling it to your entire library becomes much easier.

And I'd love to run a proper mock backend interview with you tomorrow, using your actual project and progressively harder questions.

🎯Schedule a Go streaming mock interview tomorrowTap to set up