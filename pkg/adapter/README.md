# Pixel Brite adapter spec

An adapter adds a source to a Pixel Brite server, such as a streaming service.
The server runs it as a separate program and asks it over HTTP for search
results, lists, lyrics, what to play next, and audio. The apps show its setup
steps.

Adapters don't know about Pixel Brite's features. Music, Karaoke and the rest
use whatever adapters provide. Make one adapter per source and name it after
the source.

You can write an adapter in any language. In Go, use this module: implement
`adapter.Adapter` and the interface for each capability you serve, then call
`adapter.Main`.

```go
package main

import "pixelbrite.site/adapter"

func main() {
	adapter.Main(func(dir string) adapter.Adapter { return newMyAdapter(dir) })
}
```

`pixelbrite-adapter check PATH` tests an adapter against this spec without a
server. Install it from this folder with `go install ./cmd/pixelbrite-adapter`.
Each run is a new member in a new data folder, set up, reset and forgotten.
Against a real account, pass `-data DIR`: the adapter keeps its data there
between runs, the member stays set up and is neither reset nor forgotten, so
every run is the same member in the same browser.
A Pixel Brite server has the same check as `pixelbrite adapter check PATH`, and
`pixelbrite adapter spec` prints the version of this spec it implements.

## The adapter folder

An adapter is a folder, named after its ID, that holds `adapter.json` and a
program for each platform:

```json
{
  "id": "tones",
  "name": "Tones",
  "version": "1.0.0",
  "capabilities": {"status": "1", "search": "1", "live": "1"},
  "live": {"pace": "reader", "streams": 0, "seek": true},
  "programs": {"darwin-arm64": "tones", "linux-amd64": "linux-amd64/tones", "windows-amd64": "tones.exe"}
}
```

| Field | Meaning |
|---|---|
| `id` | Lowercase letters, digits and hyphens. The server's API uses it to name the adapter. `library` is taken by the server's own music. |
| `name` | Shown in the apps. |
| `version` | Any text. The server shows it, and when updating only checks whether it changed. |
| `capabilities` | Each capability you serve and the versions of it you speak: `"1"` or `"1-3"`. `status` is required. |
| `live` | Required if you serve `live`. See [Live audio](#live-audio). |
| `programs` | The program's path in the folder, with `/` separators, for each platform you support: `darwin-arm64`, `darwin-amd64`, `linux-amd64`, `linux-arm64`, `windows-amd64`, `windows-arm64` (`GOOS-GOARCH`). On Windows, name the `.exe`. |
| `requires` | Optional. Programs the server provides to you, from the list below. |

The server provides these programs to adapters that list them in `requires`:

| Name | Program |
|---|---|
| `chrome` | Google Chrome. The server installs one copy, shared by every adapter that requires it, when the owner presses the install step it adds to your status; it uses the Chrome already installed on the computer when there is one. |

The program's path reaches you in the environment variable `PIXELBRITE_` and
the name in capitals, hyphens as underscores: `PIXELBRITE_CHROME`. It is unset
until the program is installed. The server adds an owner step for it at the
start of your status and reports you not ready until it is there, then
restarts you with the variable set. A server that does not provide a name you
require does not start you and shows the owner that you need a newer server.

The server reads `adapter.json` without running anything. If there's no
program for the server's platform, the adapter shows as unavailable there.

## Packages

A server owner installs an adapter by giving the server a URL. The URL returns
a package, or an index listing one package per platform so the server
downloads only its own:

```json
{"packages": {"darwin-arm64": "https://example.com/tones-1.0.0-darwin-arm64.tar.gz",
              "linux-amd64": "tones-1.0.0-linux-amd64.tar.zst"}}
```

Relative URLs in an index resolve against the index URL. Downloads use HTTPS.

A package is a tar or zip archive, optionally compressed with gzip, zstd, xz or
bzip2. The server tells the format from the contents, not the file name or
content type. Put `adapter.json` at the top of the archive or inside a single
top-level folder. One package can hold programs for several platforms. The
server won't unpack entries or links that point outside the package, or device
files. There are no checksums; TLS covers integrity.

## Starting and stopping

The server runs the program for its platform like this:

```
PROGRAM -listen SOCKET -data DIR -capabilities status=1,search=1,live=1
```

- `SOCKET` is a Unix socket path. Listen on it and serve the HTTP requests
  below. The requests don't depend on the socket, so they work over any byte
  stream.
- `DIR` is your data folder. It's kept across restarts and updates, and the
  program starts in it.
- `-capabilities` is the version the server picked for each capability: the
  newest one you both speak. A capability with no version in common is left
  out.

Then print one line to stdout and flush it (stdout is usually buffered when
it isn't a terminal):

- `ready` once you accept connections on `SOCKET`.
- `incompatible REASON` if you can't serve a capability at the version given.
  Then exit.

The server waits 30 seconds for that line and ignores stdout after it. Stderr
goes to the server's log.

The server keeps your stdin open. When it closes, stop everything you started
and exit, so you never outlive the server. To stop you (turned off, updated,
removed, or the server shutting down), the server closes stdin, waits 5
seconds, then kills you. On macOS and Linux you run as the leader of your own
process group, and the server kills the whole group once you've exited or been
killed, so leftover helpers go too. If you use a launcher script, `exec` the
program so it gets stdin.

You may download helper programs into your data folder and start child
processes. If you exit, the server restarts you, waiting from 1 second up to 1
minute between attempts while you keep failing. It doesn't restart an adapter
that answered `incompatible`. An adapter that stops answering `/status` (see
below) is stopped and restarted the same way.

## Versions

Each capability has its own version. All are currently 1. A change that
existing adapters or servers couldn't handle adds a new version of that
capability, and both sides keep speaking the older versions they still
support.

These changes don't add a version, so be ready for them:

- New fields in a JSON object. Ignore fields you don't know.
- New setup step kinds and item kinds.
- New capabilities.

## Members

The server makes requests for the profiles signed in to it. A request for a
member has a `Member` header: a random handle for that profile and this
adapter, 32 lowercase hex characters, safe to use as a folder name. It's never
a user ID, email or token, and it doesn't change. A request without the header
is for the server itself. Shared profiles (guest mode, the public demo) never
get a handle. When a profile is deleted, the server sends `DELETE /member` with
its handle, and you delete everything you keep for it.

If you have member steps, keep each member's state separate by handle, and
serve several members at once when each has their own account. Don't
authenticate requests; the server decides who can make them.

## HTTP requests

Requests and responses are JSON unless noted. Errors use a 4xx or 5xx status
and `{"message": "..."}`. 404 means the item doesn't exist, or for lyrics, that
there are none. The apps show setup and playback error messages; the rest go
to the server's log.

### `status` capability

#### `GET /status`

```json
{"revision": 7, "status": {"name": "Tones", "ready": false, "setup": [Step]}}
```

- `revision` goes up with every change to any member's status.
- `name` is the display name.
- `ready` means you can serve this member now. Without a member, it means you
  can serve members who finished their steps.
- `unavailable`, if present, is one sentence saying why you can't run on this
  computer. The apps show it instead of setup steps; leave `setup` out.
- `setup` lists, in order, what it takes to become ready. Leave it out if
  there's nothing to set up. The server's own view (no `Member` header) lists
  every step, owner and member alike. That's how the server learns who does
  each step, and it refuses steps it hasn't seen there.

With `?after=R`, wait until `revision` is greater than `R` and answer then, or
answer after 25 seconds. The server keeps one of these open without a member,
and when the revision rises it asks again for the members it's showing. If
that request goes unanswered for 35 seconds, the server treats the adapter as
stopped. A request without `after` must answer within 3 seconds, or the apps
show the adapter as not answering.

A `Step`:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Used in `POST /setup/{id}`. |
| `kind` | string | `web_sign_in`, `secret` or `action`. |
| `audience` | string | `owner`: done once for the server, by an owner. `member`: each member does it for their own account. |
| `title` | string | What the step asks for. |
| `button` | string | The button label. |
| `detail` | string, optional | More explanation. For `secret`, the text of the link to `url`. |
| `url` | string, optional | For `web_sign_in`, the sign-in page (HTTPS). For `secret`, where to get the value. |
| `done` | bool | The step is done for this member (for owner steps, for the server). |
| `busy` | bool, optional | An `action` is running. |
| `error` | string, optional | Why the last attempt failed (a pairing timed out, the service ended the session), until it's tried again. The apps show it. |

Kinds:

- `web_sign_in`: the app opens `url` in a private browser session. When the
  person is done signing in, the app sends every cookie the session holds to
  `POST /setup/{id}` as the value, a JSON array:

  ```json
  [{"name": "sid", "value": "…", "domain": ".example.com", "path": "/", "expires": "2027-01-01T00:00:00Z", "secure": true, "http_only": true}]
  ```

  A `domain` that starts with a dot also covers subdomains. A cookie without
  `expires` lasts only for the session. Answer 204 if the cookies sign them
  in, or an error saying why; the app shows it.
- `secret`: the person types a value, such as an API key.
- `action`: a button. While `busy`, `detail` says what to do in the meantime.

#### `POST /setup/{id}`

Body `{"value": "..."}`, empty for `action` steps. A member step carries the
member's handle. Answer 204, or an error whose message says what went wrong.
Raise the revision to announce the new state.

#### `DELETE /setup/{id}`

Undo a finished step so it can be done again: sign out, remove a saved key, or
forget a pairing. A member step carries the member's handle. Answer 204 or an
error, and raise the revision.

#### `DELETE /member`

Carries the handle of a deleted profile. Stop whatever you run for that member
and delete everything you keep for them. Answer 204.

### Items

Lists hold typed items:

```json
{"kind": "track", "id": "…", "title": "…", "artist": "…", "album": "…", "duration_ms": 215005, "artwork": "https://…"}
```

- `kind` is `artist`, `album`, `track`, `playlist`, `folder`, `video` or
  `file`. Ignore kinds you don't know.
- `id` is unique within your adapter and stays the same: the server passes it
  back to browse, lyrics and play, possibly much later. It can be any
  non-empty text except `.` and `..`. Requests carry it percent-encoded as one
  path segment (`/` arrives as `%2F`), or as a query value.
- `artwork` is an absolute URL. The server fetches and serves it.
- Fill in a track's `title`, `artist`, `album` and `duration_ms` accurately.
  If you have no lyrics, the server uses them to look lyrics up.

### `search` capability: `GET /search?q=QUERY`

Answer `{"items": [Item]}`, best match first, or an empty list. Tracks alone
are fine.

### `browse` capability: `GET /browse?id=ID&cursor=CURSOR`

List what's in item `id` (an artist, album, playlist or folder), or your top
level when there's no `id`. Answer `{"items": [Item], "next": "CURSOR"}`. The
server passes `next` back as `cursor` for the next page. Leave `next` out on
the last page.

### `lyrics` capability: `GET /lyrics/{id}`

```json
{"synced": true, "lines": [{"start_ms": 12000, "words": "…"}]}
```

For untimed text, set `synced` to false and every `start_ms` to 0. Answer 404
if you have no lyrics for the track, and the server looks elsewhere.

### `continuation` capability: `GET /continuation?played=ID&played=ID`

Answer `{"items": [Item]}`: what to play after the listed items, which come
oldest first.

### `live` capability: `GET /play/{id}`

Play the track and return when it ends.

- If it fails before any audio, answer a 4xx or 5xx with a message.
- Otherwise answer 200 with a `Sample-Rate` header (Hz, such as `44100`) and
  a body of records. Each record is a type byte, the payload's length as a
  4-byte little-endian unsigned integer, and the payload:
  - `a`: audio, the next bytes of interleaved stereo float32 little-endian
    frames.
  - `e`: the end, always the last record. Its payload is a JSON object: `{}`
    when the track played to the end, or `{"message": "..."}` when it failed.

  In Python, a record is `kind + struct.pack("<I", len(payload)) + payload`.
  The server skips record types it doesn't know. A body that stops before its
  `e` record counts as a failure, so any HTTP framing works: chunked, a
  `Content-Length`, or closing the connection.
- The server closes the connection to stop a track early. Stop playing then.
- If you declare `seek`, `?start_ms=N` starts N milliseconds in.

## Live audio

`live` in `adapter.json`:

| Field | Meaning |
|---|---|
| `pace` | `reader`: you produce audio as fast as the server reads it, and the connection's flow control paces you. `realtime`: you produce audio at playback speed no matter what, and anything the server hasn't read within your buffer is lost. |
| `streams` | How many tracks you can play at once for one member; 0 means no limit. The server refuses more, across all features. |
| `seek` | Whether `/play` takes `start_ms`. Without it, tracks play from the start. |
