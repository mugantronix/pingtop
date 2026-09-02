// Package clipboard reads the system clipboard. It is a thin wrapper
// around github.com/atotto/clipboard so the rest of the codebase
// depends on a local interface rather than a third-party package
// directly — swapping implementations later (or stubbing in tests)
// only touches this file.
package clipboard

import "github.com/atotto/clipboard"

// Read returns the current text contents of the system clipboard. On
// Windows and macOS this talks to the OS clipboard API directly; on
// Linux it shells out to xclip/xsel/wl-clipboard (whichever is
// available) since there's no single userspace API. An error there
// (e.g. no clipboard tool installed, or a headless session with no
// clipboard at all) is returned as-is for the caller to decide how to
// surface it.
func Read() (string, error) {
	return clipboard.ReadAll()
}
