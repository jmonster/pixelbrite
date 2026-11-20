# A Spotify adapter

Here's how you could build an adapter that lets Pixel Brite search and play
Spotify, for example to sing Spotify songs at a karaoke party. It would serve
`status`, `search`, `lyrics` and `live`.

## Setup

Each member needs their own Spotify account. The adapter would need three
things from each member, stored under their `Member` handle:

1. Their Spotify session. A `web_sign_in` step on open.spotify.com would hand
   the adapter the member's cookie jar once they sign in. With the `sp_dc`
   cookie from it, the adapter could get access tokens the way the web player
   does.
2. A Spotify Soloist key, collected with a `secret` step. Soloist is Spotify's
   headless player. The adapter would download it to its data folder and use
   it to play tracks.
3. Soloist linked to their account. An `action` step could start Soloist and
   wait until the member picks it as a device in the Spotify app.

`DELETE /setup/{id}` would undo a step, and `DELETE /member` would stop the
member's Soloist and delete their data.

## Search and lyrics

Both could use the web player's API with the member's access token. Return
each track's real `duration_ms`.

## Playback

For `GET /play/{id}`, the adapter would have the member's Soloist play the
track and capture its audio with a Core Audio process tap (macOS 14.2 or
later). The samples are already float32 stereo, so they can go straight into
audio records.

Soloist's local WebSocket reports the track's position and when it ends or
fails. The adapter would use that to send the end record. When the server
closes the connection, it would stop Soloist.

Soloist plays in real time, and an account can only play one track at a time,
so `adapter.json` would declare:

```json
"live": {"pace": "realtime", "streams": 1, "seek": false}
```

Pixel Brite then handles these songs like any others.
