//go:build !windows

package browser

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

func waitDocumentProcessGroup(ctx context.Context, pgid int) error {
	if pgid <= 1 || pgid == syscall.Getpgrp() {
		return errors.New("document process group ownership unavailable")
	}
	ticker := time.NewTicker(documentReapPoll)
	defer ticker.Stop()
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("document verify process group: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("document process group did not exit: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
