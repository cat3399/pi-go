// Package rpc implements pi's headless JSONL protocol above an
// application.ApplicationSession. It is deliberately a wire adapter: all
// product state and behavior remain owned by Runtime -> AgentSession -> Agent.
package rpc

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	protocolv1 "github.com/cat3399/pi-go/internal/protocol/v1"
)

type commandEnvelope struct {
	ID   *string `json:"id"`
	Type string  `json:"type"`
}

type decodedCommand struct {
	id      *string
	typ     string
	command application.Command
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
	command, err := protocolv1.DecodeCommand(line, agent.InputRPC)
	if err != nil {
		return decoded, err
	}
	decoded.command = command
	return decoded, nil
}
