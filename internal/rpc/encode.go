package rpc

import (
	"github.com/cat3399/pi-go/internal/host"
	"github.com/cat3399/pi-go/internal/hostjson"
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

func encodeResult(result host.CommandResult) (any, error) {
	data, present, err := hostjson.EncodeResult(result)
	if err != nil {
		return nil, err
	}
	if !present {
		return omittedData, nil
	}
	return data, nil
}

func encodeHostEvent(event host.Event) (any, error) {
	return hostjson.EncodeEvent(event)
}
