package browser

import (
	"fmt"
	"mime"
	"strings"
)

// DocumentContentTypeError is ambiguous or invalid observed response metadata.
// It preserves the observed value and is deliberately distinct from a supported
// header naming an unsupported representation, which may be safely rerouted.
type DocumentContentTypeError struct {
	Value string
	Err   error
}

func (e *DocumentContentTypeError) Error() string {
	return fmt.Sprintf("invalid document Content-Type %q: %v", e.Value, e.Err)
}
func (e *DocumentContentTypeError) Unwrap() error { return e.Err }

// CDP joins repeated header fields using LF. Accept equivalent declarations,
// merging non-conflicting parameters without dropping unknown ones. Only media
// types, parameter names and charset labels have case-insensitive semantics;
// values such as multipart boundaries must retain their exact case.
func normalizeDocumentContentType(raw string) (string, error) {
	var mediaType string
	merged := make(map[string]string)
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		typ, params, err := mime.ParseMediaType(strings.TrimSpace(line))
		if err != nil {
			return "", &DocumentContentTypeError{Value: raw, Err: err}
		}
		if !strings.Contains(typ, "/") {
			return "", &DocumentContentTypeError{Value: raw, Err: fmt.Errorf("media type requires type/subtype")}
		}
		if mediaType != "" && mediaType != typ {
			return "", &DocumentContentTypeError{Value: raw, Err: fmt.Errorf("conflicting media types %q and %q", mediaType, typ)}
		}
		mediaType = typ
		for key, value := range params {
			if key == "charset" {
				value = strings.ToLower(value)
			}
			if previous, exists := merged[key]; exists && previous != value {
				return "", &DocumentContentTypeError{Value: raw, Err: fmt.Errorf("conflicting %s parameter", key)}
			}
			merged[key] = value
		}
	}
	return mime.FormatMediaType(mediaType, merged), nil
}
