package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func runOnceResult(args []string) (CommandResult, error) {
	id := fmt.Sprintf("once-%d-%d", os.Getpid(), time.Now().UnixNano())
	create := []string{}
	observe := []string{"--id", id}
	for i := 0; i < len(args); i++ {
		key, _, inline := strings.Cut(args[i], "=")
		observationFlag := key == "--content" || key == "--screenshot" || key == "--health" || key == "--tree" || key == "--all" || key == "--annotate"
		if observationFlag {
			observe = append(observe, args[i])
			if (key == "--screenshot" || key == "--content") && !inline && i+1 < len(args) {
				i++
				observe = append(observe, args[i])
			}
		} else {
			create = append(create, args[i])
		}
	}
	create = append(create, "--id", id)
	opened, e := runOpenResult(create)
	if e != nil || opened.Code != exitOK {
		return opened, e
	}
	observed, observeErr := runObserveResult(observe)
	closed, closeErr := runCloseResult([]string{"--id", id})
	if closeErr != nil || closed.Code != exitOK {
		return commandResult(map[string]any{"success": false, "phase": "cleanup", "observation": observed.Payload, "cleanup": closed.Payload}, exitRunErr), deliveryCommandError(fmt.Errorf("once session cleanup failed: %v", closeErr))
	}
	return observed, observeErr
}
