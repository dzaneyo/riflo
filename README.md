# riflo

riflo is a small local tool for inspecting and downloading media with FFprobe and FFmpeg.

It provides:

- a local Web UI;
- HLS playlist inspection and explicit quality selection;
- background downloads with progress and history;
- bounded retries for temporary HLS and HTTP failures;
- direct CLI inspection and downloads;
- a Chrome and Firefox extension that discovers HLS candidates and replays safe browser request context.

## Requirements

- Go 1.27+
- FFmpeg and FFprobe

## Build and run

```bash
go test ./...
go build -o dist/riflo ./cmd/riflo
./dist/riflo doctor
./dist/riflo serve
```

Open <http://127.0.0.1:8787>.

## CLI

```bash
./dist/riflo inspect 'https://example.com/video.m3u8'
./dist/riflo download 'https://example.com/video.m3u8'
./dist/riflo tasks
```

Use `--referer`, `--origin`, `--user-agent`, or `--cookie` when the media server requires normal request context. Use `--hls-variant-index` to select a variant reported by `inspect`. These values are used only for the current request and are not stored.

## Browser extension

Start `riflo serve`, then load the `extension` directory as an unpacked extension:

- Chrome: `chrome://extensions`
- Firefox: `about:debugging#/runtime/this-firefox`

Play a video, open the extension, and send a detected HLS candidate to riflo. It can replay Referer, Origin, and User-Agent, but it does not read cookies or start downloads automatically.

riflo does not bypass DRM, authentication, paywalls, or other access controls. Only download media you are authorized to access.
