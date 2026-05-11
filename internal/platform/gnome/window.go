package gnome

import (
	"errors"
)

// IsExtensionUnreachable reports whether err indicates the vocis-gnome
// extension is not installed/enabled (vs. some other failure).
func IsExtensionUnreachable(err error) bool {
	return errors.Is(err, ErrExtensionNotInstalled)
}
