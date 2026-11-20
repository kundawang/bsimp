# Bsimp

Bsimp is a minimalistic S3-backed audio library. It lets you play audio files from an S3 bucket with any arbitrary directory structure.

It works with AWS S3 or any S3 API compatible storage such as DigitalOcean Spaces, Backblaze B2, Cloudflare R2 or MinIO.

## Why

Over the years I acquired a large library of audio files from different sources - Bandcamp, Google Music and I even ripped some CDs myself a decade ago. I wanted a way to listen my audio files from different devices and also have them backed up reliably on cloud storage. S3 solves both of these problems - it serves as a live audio library and as a backup at the same time.

I didn't want to go with the existing [open source](https://github.com/awesome-selfhosted/awesome-selfhosted#media-streaming---audio-streaming) audio streaming services. I found them resource-heavy, having many dependencies and features I would never use.

## Features

- Cover art support
- Tag indexing: reads artist, album, title and track number, groups tracks into albums and adds search regardless of file names
- Responsive design
- Stateless - no database required

## Screenshots

Directory Browser

<img src="docs/directory-browser.png" alt="directory-browser" width="631"/>

Audio Player

<img src="docs/player.png" alt="player" width="631"/>

## Configuring

DigitalOcean Spaces config example:

```toml
[s3]
region = "nyc3"
endpoint = "https://nyc3.digitaloceanspaces.com"
bucket = "foo"

[s3.credentials]
id = "SPACES KEY"
secret = "SPACES SECRET"
```

MinIO config example:
```toml
[s3]
region = "local"
endpoint = "http://localhost:9000"
bucket = "music"
force_path_style = true

[s3.credentials]
id = "minioadmin" 
secret = "minioadmin"
```

## Running

```sh
bsimp -config=/etc/bsimp/config.toml -http=":8080"
```

## Tag indexing

Bsimp reads ID3v1/ID3v2, MP4, FLAC, Ogg Vorbis and Opus tags directly from the
object store. Only the leading metadata bytes of each object are fetched, using
HTTP range requests, so the audio itself is never downloaded.

- **Albums** (`/search` and `/album/<id>`) group tracks by album title and album
  artist, so two different "Greatest Hits" albums do not merge and the same
  album stays together even when its files are scattered across directories or
  named unpredictably.
- **Fallbacks**: a missing, damaged, oversized or unreadable tag never hides a
  track. The title falls back to the file name, the album to the containing
  directory, and the track number to a leading number in the file name
  (`01. Song`, `3 - Song`, `1-02 Song`). Search and the directory listing mark
  such tracks as `untagged`.
- **Search** is case- and Unicode-normalization-insensitive. All whitespace
  separated terms must match. Results are bounded (`limit`, default 100, max
  500) and report total match counts plus whether they were capped.
- **Incremental**: after the first scan, an object is re-read only when its
  ETag or size changes. Transient read errors are not cached and are retried on
  the next scan.

The index lives in memory only - nothing is written to the bucket. Index status
and manual refresh are available at `/index/`.

Index behavior can be tuned:

```toml
[index]
# Audio objects indexed per snapshot. The scan stops at the cap and reports
# a truncated index. Set to -1 for no cap.
max_objects = 50000
# Concurrent tag reads.
concurrency = 8
# Per-object and per-scan deadlines.
tag_timeout = "10s"
scan_timeout = "15m"
# How long a snapshot is reused before a background rescan.
refresh_interval = "5m"
# Bytes and range requests allowed per object when reading tags.
max_tag_bytes = 1048576
max_tag_requests = 64
```

Because tag parsing runs over untrusted object bytes, every read is bounded by
`max_tag_bytes` and `max_tag_requests`, each read has its own deadline, and a
panicking parser is reported as a corrupt tag instead of crashing the scan. If a
scan fails outright (for example the bucket is unreachable), the previous
snapshot keeps being served and the failure is shown on `/index/`.

## Security

Bsimp doesn't have built-in authentication or rate-limiting. The server should never be exposed to the Internet directly to avoid unexpected S3 bills.

When exposed to the Internet, the server should run behind a full-fledged web server like Nginx with the following features enabled:
- HTTPS
- Authentication
- Rate Limiting

## FAQ

### What audio formats does it support?

All audio formats [supported](https://caniuse.com/?search=audio%20format) by the web browser.

### Is there a mobile app?

No, but the web interface works well on mobile phones. The Media Session API lets you control the playback from the notification bar or the lock screen.

### Does it support playlists?

No, Bsimp follows the S3 bucket directory structure for browsing. The tag index
adds album grouping and search on top, but does not define playlists.

### Does it support transcoding?

No, audio files are streamed as is.
