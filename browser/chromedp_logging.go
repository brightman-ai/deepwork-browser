package browser

import (
	"fmt"
	"log"
	"strings"
)

func chromedpErrorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if strings.Contains(msg, "unhandled node event *dom.EventTopLayerElementsUpdated") {
		return
	}
	log.Printf("ERROR: %s", msg)
}
