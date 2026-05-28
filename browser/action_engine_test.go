package browser

import "testing"

func TestMapKeyNameIsCaseInsensitiveForCommonKeys(t *testing.T) {
	tests := map[string]string{
		"enter":        "\r",
		"Return":       "\r",
		"esc":          "\u001b",
		"arrowdown":    "\ue015",
		"ctrl+enter":   "\ue009\r",
		"Shift+tab":    "\ue008\t",
		"f5":           "\ue035",
		"plain-letter": "plain-letter",
	}

	for input, want := range tests {
		if got := mapKeyName(input); got != want {
			t.Fatalf("mapKeyName(%q) = %q, want %q", input, got, want)
		}
	}
}
