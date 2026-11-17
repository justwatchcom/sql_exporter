//go:build !race

package gosnowflake

import (
	"encoding/json"
	"io"
)

// decodeJSONBody decodes one JSON value from body.
// The race-detector build buffers the body first; see restful_decode_race.go.
func decodeJSONBody(body io.Reader, v any) error {
	return json.NewDecoder(body).Decode(v)
}
