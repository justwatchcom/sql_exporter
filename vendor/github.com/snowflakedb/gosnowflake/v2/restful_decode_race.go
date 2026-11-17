//go:build race

package gosnowflake

import (
	"bytes"
	"encoding/json"
	"io"
)

// decodeJSONBody reads the whole body before decoding it.
// Since Go 1.27, decoding a large value straight from a network stream
// re-copies the buffered value on every read, which is quadratic in the
// value size under the race detector.
func decodeJSONBody(body io.Reader, v any) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	return json.NewDecoder(bytes.NewReader(b)).Decode(v)
}
