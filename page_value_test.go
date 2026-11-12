package browserkit

import (
	"errors"
	"testing"
)

func TestValidateValueInput(t *testing.T) {
	valid := []struct {
		kind string
		text string
	}{
		{"date", ""},
		{"date", "2026-10-21"},
		{"month", "2026-10"},
		{"week", "2026-W42"},
		{"time", "23:59"},
		{"time", "23:59:58"},
		{"time", "23:59:58.123"},
		{"datetime-local", "2026-10-21T23:59"},
		{"datetime-local", "2026-10-21T23:59:58"},
		{"datetime-local", "2026-10-21T23:59:58.123"},
		{"color", "#A1b2C3"},
		{"range", "5"},
		{"range", "5.5"},
		{"range", "5.0"},
		{"range", "1e3"},
		{"range", ".5"},
		{"range", "0.0000000000005"},
		{"number", "0"},
		{"number", "-1.5"},
		{"number", "1e3"},
		{"number", ""},
	}
	for _, test := range valid {
		if err := validateValueInput(test.kind, test.text); err != nil {
			t.Fatalf("validateValueInput(%s, %q)=%v", test.kind, test.text, err)
		}
	}

	invalid := []struct {
		kind string
		text string
	}{
		{"date", "0000-01-01"},
		{"month", "0000-01"},
		{"date", "2026-02-30"},
		{"date", "2026/10/21"},
		{"month", "2026-13"},
		{"week", "2026-W54"},
		{"week", "2026-42"},
		{"time", "25:00"},
		{"datetime-local", "2026-02-30T10:00"},
		{"datetime-local", "0000-01-01T10:00"},
		{"color", ""},
		{"color", "red"},
		{"color", "#12345"},
		{"range", ""},
		{"range", "abc"},
		{"range", "Inf"},
		{"range", "5."},
		{"range", "+5"},
		{"range", " 5 "},
		{"range", "0x10"},
		{"number", "abc"},
		{"number", "NaN"},
	}
	for _, test := range invalid {
		err := validateValueInput(test.kind, test.text)
		var browserErr *Error
		if err == nil || !errors.As(err, &browserErr) || browserErr.Kind != "invalid_value" {
			t.Fatalf("validateValueInput(%s, %q)=%v，期望 invalid_value", test.kind, test.text, err)
		}
	}
}
