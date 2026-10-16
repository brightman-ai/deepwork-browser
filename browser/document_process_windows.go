//go:build windows

package browser

import (
	"context"
	"errors"
)

func waitDocumentProcessGroup(context.Context, int) error {
	return errors.New("document process group verification unsupported on Windows")
}
