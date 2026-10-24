package surfacewire

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
)

func EncodeEventCursor(cursor application.EventCursor) string {
	if cursor.Epoch == "" {
		return ""
	}
	return cursor.Epoch + ":" + strconv.FormatUint(cursor.Sequence, 10)
}

func DecodeEventCursor(value string) (*application.EventCursor, error) {
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[0]) > 128 {
		return nil, ErrInvalidRequest
	}
	for _, r := range parts[0] {
		if r < '0' || r > '9' && r < 'A' || r > 'Z' && r < 'a' || r > 'z' {
			return nil, ErrInvalidRequest
		}
	}
	sequence, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: event cursor", ErrInvalidRequest)
	}
	return &application.EventCursor{Epoch: parts[0], Sequence: sequence}, nil
}
