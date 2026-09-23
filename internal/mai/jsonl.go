package mai

import (
	"encoding/json"
	"io"
)

func writeJSONLEvent(w io.Writer, event map[string]any) error {
	return json.NewEncoder(w).Encode(event)
}

type jsonlTextWriter struct {
	output io.Writer
}

func (writer jsonlTextWriter) Write(p []byte) (int, error) {
	if err := writeJSONLEvent(writer.output, map[string]any{"type": "model.delta", "text": string(p)}); err != nil {
		return 0, err
	}
	return len(p), nil
}
