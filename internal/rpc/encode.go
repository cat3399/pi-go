package rpc

import (
	"github.com/cat3399/pi-go/internal/application"
	protocolv1 "github.com/cat3399/pi-go/internal/protocol/v1"
)

type noData struct{}

var omittedData = noData{}

func successResponse(id *string, command string, data any) map[string]any {
	response := map[string]any{"type": "response", "command": command, "success": true}
	if id != nil {
		response["id"] = *id
	}
	if _, omitted := data.(noData); !omitted {
		response["data"] = data
	}
	return response
}

func errorResponse(id *string, command string, err error) map[string]any {
	response := map[string]any{"type": "response", "command": command, "success": false, "error": err.Error()}
	if id != nil {
		response["id"] = *id
	}
	return response
}

func encodeResult(result application.CommandResult) (any, error) {
	data, present, err := protocolv1.EncodeResult(result)
	if err != nil {
		return nil, err
	}
	if !present {
		return omittedData, nil
	}
	return data, nil
}

func encodeApplicationEvent(event application.Event) (any, error) {
	return protocolv1.EncodeEvent(event)
}
