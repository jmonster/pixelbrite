// Command pixelbrite-adapter prints the Pixel Brite adapter spec and tests an
// adapter against it without a server:
//
//	pixelbrite-adapter spec
//	pixelbrite-adapter check [-query TEXT] [-setup STEP=VALUE]... [-audio DURATION] [-quiet] PATH
//
// A Pixel Brite server has the same commands as `pixelbrite adapter`.
package main

import (
	"os"

	"pixelbrite.site/adapter/check"
)

func main() {
	os.Exit(check.Command("pixelbrite-adapter", os.Args[1:], os.Stdout, os.Stderr))
}
