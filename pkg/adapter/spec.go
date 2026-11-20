package adapter

import _ "embed"

// Spec is the adapter spec this module implements: its README.md, which
// `pixelbrite adapter spec` prints.
//
//go:embed README.md
var Spec string
