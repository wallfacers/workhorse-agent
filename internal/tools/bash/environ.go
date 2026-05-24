package bash

import "os"

// osEnvironReal is split into its own file so it can be replaced trivially
// without rerouting the public API.
func osEnvironReal() []string { return os.Environ() }
