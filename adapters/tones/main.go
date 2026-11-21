// Command tones is an example Pixel Brite adapter. It plays test tones it
// generates, so it needs no account or network, and it uses every part of
// the spec in pkg/adapter/README.md: a member setup step, search, paged
// browsing, lyrics, continuation, and live audio that seeks.
package main

import "pixelbrite.site/adapter"

func main() {
	adapter.Main(func(dir string) adapter.Adapter { return newTones(dir) })
}
