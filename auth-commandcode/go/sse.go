package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
)

type sseEvent struct {
	Event string
	Data  string
}

func readSSEEvent(reader *bufio.Reader) (sseEvent, error) {
	var event sseEvent
	var data []string
	gotField := false
	for {
		line, errRead := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) == 0 {
				if !gotField && errRead != nil {
					return sseEvent{}, errRead
				}
				event.Data = strings.Join(data, "\n")
				return event, nil
			}
			gotField = true
			text := string(line)
			if strings.HasPrefix(text, ":") {
				continue
			}
			field, value, found := strings.Cut(text, ":")
			if found {
				value = strings.TrimPrefix(value, " ")
			} else {
				field = text
				value = ""
			}
			switch field {
			case "event":
				event.Event = value
			case "data":
				data = append(data, value)
			}
		}
		if errRead != nil {
			if !gotField {
				return sseEvent{}, errRead
			}
			event.Data = strings.Join(data, "\n")
			if event.Data == "" && event.Event == "" {
				return sseEvent{}, errRead
			}
			return event, nil
		}
	}
}

func isSSEDone(event sseEvent) bool {
	return strings.TrimSpace(event.Data) == "[DONE]"
}

func newSSEReader(r io.Reader) *bufio.Reader {
	return bufio.NewReaderSize(r, 32*1024)
}

func fmtSSEError(event sseEvent) string {
	if strings.TrimSpace(event.Data) == "" {
		return fmt.Sprintf("upstream stream error event %q", event.Event)
	}
	return event.Data
}
