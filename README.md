# Nugs-Downloader
Nugs downloader written in Go.

This is a fork of [GavinGoGaming/Nugs-Downloader](https://github.com/GavinGoGaming/Nugs-Downloader), which patched and compiled [Sorrow446/Nugs-Downloader](https://github.com/Sorrow446/Nugs-Downloader), the original. Thanks to both.

![](https://i.imgur.com/NOsQjnP.png)
![](https://i.imgur.com/BEudufy.png)

# Downloads
Ready-to-run builds for Windows, macOS, Linux, FreeBSD and Android (Termux) are on the [releases page](https://github.com/vincentmele/Nugs-Downloader/releases). Each is a single file that needs no other software. Unpack the archive for your system and put `config.json` and `.env` next to `nugs_dl` (see Setup).

On macOS, if the system won't run it because it was downloaded from the internet, run `xattr -d com.apple.quarantine nugs_dl` once.

# Building
Requires Go (see `go.mod` for the version).
```
go build
go test ./...
```
This produces `nugs_dl` (`nugs_dl.exe` on Windows). It's pure Go, so it cross-compiles to a single self-contained file for any platform, e.g. `GOOS=windows GOARCH=amd64 go build` or `GOOS=linux GOARCH=arm64 go build`.

`./release.sh v1.2.3` builds the downloads for every platform into `dist/`, and `./release.sh v1.2.3 --publish [notes.md]` also uploads them as a GitHub release (needs the `gh` CLI, signed in).

# Setup
Put these files next to the binary.

## Credentials
Copy `.env.example` to `.env` and fill in your account details. Keep it private (`chmod 600 .env` on macOS/Linux); it's ignored by git. Variables set in the environment take precedence over `.env`.

|Variable|Info|
| --- | --- |
|NUGS_EMAIL|Email address.
|NUGS_PASSWORD|Password.
|NUGS_TOKEN|Token to auth with Apple and Google accounts ([how to get token](token.md)). Used instead of the email and password when set; leave empty if you're using a regular account. A token lasts about 10 hours and can't be renewed by the program, while an email and password login is refreshed automatically during long runs.

Credentials in `config.json` still work if none are set in `.env` or the environment, but are deprecated.

## Options
Copy `config.example.json` to `config.json` and configure any options if needed.

|Option|Info|
| --- | --- |
|format|Track download quality. 1 = 16-bit / 44.1 kHz ALAC, 2 = 16-bit / 44.1 kHz FLAC, 3 = 24-bit / 48 kHz MQA, 4 = 360 Reality Audio / best available, 5 = 150 Kbps AAC.
|videoFormat|Video download format. 1 = 480p, 2 = 720p, 3 = 1080p, 4 = 1440p, 5 = 4K / best available.
|outPath|Where to download to. Path will be made if it doesn't already exist.
|folderFormat|Where each release goes inside outPath. `/` makes folders, and `{artist}`, `{date}` (the show or release date as YYYY-MM-DD), `{title}` (the venue for shows, the name for albums) and `{container}` (the name as Nugs gives it) are filled in. Default `{artist}/{date} {title}`, e.g. `Goose/2026-09-02 Dillon Amphitheater, Dillon, CO`. Use `{artist} - {container}` for the layout of older versions. Releases saved with that older layout are moved to the new one when downloaded again, so nothing is downloaded twice.
|useFfmpegEnvVar|Only used with `--ffmpeg`. true = call FFmpeg from environment variable, false = call from script dir.

No other software is needed: videos and HLS-only tracks are converted to MP4/M4A by the built-in converter, losslessly, with chapters, tags (title, artist, album, date, track number) and the release's cover art. Converting a video needs about as much free disk space again as the download, which is checked before downloading.

# FFmpeg (optional)
The `--ffmpeg` flag uses FFmpeg for those conversions instead, as older versions did. Use it if the built-in converter reports an unsupported codec.

[Windows (gpl)](https://github.com/BtbN/FFmpeg-Builds/releases)    
Linux: `sudo apt install ffmpeg`    
Termux `pkg install ffmpeg`
Place in Nugs DL's script/binary directory if using FFmpeg binary.

If you don't have root in Linux, you can have Nugs DL look for the binary in the same dir by setting the `useFfmpegEnvVar` option to false.

## Supported Media
|Type|URL example|
| --- | --- |
|Album|`https://play.nugs.net/release/23329`
|Artist|`https://play.nugs.net/artist/461/latest`, `https://play.nugs.net/artist/461`, `https://play.nugs.net/browse/artist/1205`
|Catalog playlist|`https://2nu.gs/3PmqXLW`
|Exclusive Livestream|`https://play.nugs.net/watch/livestreams/exclusive/30119`, `https://play.nugs.net/livestream/30119/exclusive`
|Purchased Livestream|`https://www.nugs.net/on/demandware.store/Sites-NugsNet-Site/default/Stash-QueueVideo?skuID=624598&showID=30367&perfDate=10-29-2022&artistName=Billy%20Strings&location=10-29-2022%20Exploreasheville%2ecom%20Arena%20Asheville%2c%20NC&format=liveHdStream` Wrap in double quotes on Windows. 
|User playlist|`https://play.nugs.net/#/playlists/playlist/1215400`, `https://play.nugs.net/library/playlist/1261211`
|Video|`https://play.nugs.net/watch/release/49502`, `https://play.nugs.net/#/videos/artist/1045/Dead%20and%20Company/container/27323` Wrap the latter in double quotes on Windows.
|Webcast|`https://play.nugs.net/#/my-webcasts/5826189-30369-0-624602`, `https://play.nugs.net/library/webcast/30369`

# Usage
Args take priority over the config file.

Press Ctrl+C to stop. Partly downloaded tracks are deleted. Partly downloaded videos are kept and resumed on the next run, and a video that finished downloading isn't downloaded again, even if converting it failed. The exit status is 0 when everything succeeded, 1 if any item failed, and 130 if interrupted.

Download two albums:   
`nugs_dl https://play.nugs.net/release/23329 https://play.nugs.net/release/23790`

Download a single album and from two text files:   
`nugs_dl https://play.nugs.net/release/23329 G:\1.txt G:\2.txt`

Download a user playlist and video:
`nugs_dl https://play.nugs.net/#/playlists/playlist/1215400 "https://play.nugs.net/#/videos/artist/1045/Dead%20and%20Company/container/27323"`

Re-running a download skips anything already saved without asking Nugs for it again, so it's quick to resume a big artist download.

```
 _____                ____                _           _
|   | |_ _ ___ ___   |    \ ___ _ _ _ ___| |___ ___ _| |___ ___
| | | | | | . |_ -|  |  |  | . | | | |   | | . | .'| . | -_|  _|
|_|___|___|_  |___|  |____/|___|_____|_|_|_|___|__,|___|___|_|
          |___|

Usage: nugs_dl [--format FORMAT] [--videoformat VIDEOFORMAT] [--outpath OUTPATH] [--force-video] [--skip-videos] [--skip-chapters] [--ffmpeg] URLS [URLS ...]

Positional arguments:
  URLS

Options:
  --format FORMAT, -f FORMAT
                         Track download format.
                         1 = 16-bit / 44.1 kHz ALAC
                         2 = 16-bit / 44.1 kHz FLAC
                         3 = 24-bit / 48 kHz MQA
                         4 = 360 Reality Audio / best available
                         5 = 150 Kbps AAC [default: -1]
  --videoformat VIDEOFORMAT, -F VIDEOFORMAT
                         Video download format.
                         1 = 480p
                         2 = 720p
                         3 = 1080p
                         4 = 1440p
                         5 = 4K / best available [default: -1]
  --outpath OUTPATH, -o OUTPATH
                         Where to download to. Path will be made if it doesn't already exist.
  --force-video          Forces video when it co-exists with audio in release URLs.
  --skip-videos          Skips videos in artist URLs.
  --skip-chapters        Skips chapters for videos.
  --ffmpeg               Use FFmpeg instead of the built-in converter for videos and HLS-only tracks.
  --help, -h             display this help and exit
  --version              display version and exit
  ```
 
# Disclaimer
- I will not be responsible for how you use Nugs Downloader.    
- Nugs brand and name is the registered trademark of its respective owner.    
- Nugs Downloader has no partnership, sponsorship or endorsement with Nugs.
