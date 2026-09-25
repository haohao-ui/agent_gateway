//go:build windows

package runner

import (
	"fmt"
	"os"
)

// helperPrintPgid is the Windows counterpart of the Unix helper. Process groups are
// replaced by Job Objects on Windows, so the Unix assertion does not apply; the
// mode fails loudly instead of silently succeeding if it is ever used there.
func helperPrintPgid() int {
	fmt.Fprint(os.Stderr, "the pgid helper mode is not supported on windows")
	return 9
}
