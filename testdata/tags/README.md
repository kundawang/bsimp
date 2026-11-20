# Tag fixtures

Small, fixed audio files used by `tags_test.go` and `index_test.go`. They are
committed so tests do not depend on ffmpeg or any external tool being
installed.

| File            | Container / tag                | Fields                                                        |
|-----------------|--------------------------------|---------------------------------------------------------------|
| `mp3-v23.mp3`   | MP3 / ID3v2.3                  | Xtal / Aphex Twin / Selected Ambient Works 85-92 / track 1/13 |
| `mp3-v24.mp3`   | MP3 / ID3v2.4                  | same as above                                                 |
| `id3v1.mp3`     | MP3 / empty ID3v2 + ID3v1      | Jericho / The Prodigy / Experience / track 1                  |
| `flac.flac`     | FLAC / Vorbis comment          | Hunter / Björk / Homogenic / track 1                          |
| `ogg-vorbis.ogg`| Ogg Vorbis / Vorbis comment    | Вечная мерзлота / Психохирурги / Психохирурги / track 2       |
| `ogg-opus.opus` | Ogg Opus / Vorbis comment      | andata / 坂本龍一 / async                                      |
| `m4a.m4a`       | MP4 / iTunes atoms             | Svefn-g-englar / Sigur Rós / Ágætis byrjun / track 1/10       |
| `untagged.mp3`  | MP3 / ID3v2 with only TSSE     | no title/artist/album                                         |
| `corrupt.mp3`   | 24 bytes, invalid ID3 version  | parse error                                                   |

Regenerate (any ffmpeg is fine, output only needs stable tags; the exact bytes
do not matter to the assertions):

```sh
gen() { ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "sine=frequency=440:duration=0.25:sample_rate=22050" "$@"; }

gen -c:a libmp3lame -b:a 32k -ac 1 -id3v2_version 3 \
  -metadata title="Xtal" -metadata artist="Aphex Twin" \
  -metadata album="Selected Ambient Works 85-92" -metadata track="1/13" mp3-v23.mp3
# ...and so on for the other rows.

printf 'ID3\x09\x00\x00\x00\x00\x00\x00\x00\x00garbage-data' > corrupt.mp3
```
