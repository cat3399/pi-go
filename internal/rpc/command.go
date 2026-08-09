// Package rpc implements pi's headless JSONL protocol above host.Host. It is
// deliberately a wire adapter: all product state and behavior remain owned by
// Runtime -> AgentSession -> Agent.
package rpc

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/host"
	"github.com/cat3399/pi-go/internal/hostjson"
)

type commandEnvelope struct {
	ID   *string `json:"id"`
	Type string  `json:"type"`
}

type decodedCommand struct {
	id      *string
	typ     string
	command host.Command
}

func decodeCommand(line []byte) (decodedCommand, error) {
	var envelope commandEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return decodedCommand{typ: "parse"}, fmt.Errorf("Failed to parse command: %w", err)
	}
	decoded := decodedCommand{id: envelope.ID, typ: envelope.Type}
	if strings.TrimSpace(envelope.Type) == "" {
		return decoded, fmt.Errorf("command type is required")
	}
	command, err := hostjson.DecodeCommand(line)
	if err != nil {
		return decoded, err
	}
	decoded.command = command
	return decoded, nil
}
