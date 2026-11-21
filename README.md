<img src=".github/banner.jpg" width="100%" alt="Pixel Brite">

# Pixel Brite

Play movies, TV and music in one app. Get it at
[pixelbrite.site](https://pixelbrite.site).

## Apps

- [Apple TV](https://apps.apple.com/us/app/pixel-brite/id6757208052)
- [iPhone](https://apps.apple.com/us/app/pixel-brite/id6757208052)
- [iPad](https://apps.apple.com/us/app/pixel-brite/id6757208052)
- Mac (coming soon)
- [Roku](https://channelstore.roku.com/details/845c90aeb7ca1ec7095ef40e15be5748:6de1b58f2edc4380d0e8966b02fa53c1/pixel-brite)
- Android (coming soon)
- Google TV (coming soon)
- Fire TV (coming soon)
- Samsung TV (coming soon)

## Adapters

Adapters add sources to a Pixel Brite server, like a streaming service or a TV
provider. An adapter is a small program the server runs and talks to over
HTTP. You can write one in any language.

1. Read the [spec](pkg/adapter/README.md).
2. Copy [adapters/tones](adapters/tones). It plays test tones and uses every
   part of the spec.
3. Check it:

```
go -C pkg/adapter install ./cmd/pixelbrite-adapter
pixelbrite-adapter check path/to/adapter
```

The check runs your adapter, calls everything its `adapter.json` declares, and
lists every problem. Use `-setup STEP=VALUE` to complete setup steps and
`-query` to search for something your adapter has. For tones:

```
cd adapters/tones && go build -o tones . && pixelbrite-adapter check -query a4 -setup enable= .
```

To install an adapter, give its package's HTTPS address to your server in
Settings › Adapters.

[examples/python](examples/python) is the same kind of adapter in Python,
using only the standard library.
