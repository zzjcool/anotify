package sourcespec

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateNameExistingRuleOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"", ErrEmptyName},
		{" \t\n ", ErrEmptyName},
		{"route", ErrReservedName},
		{" \troute\n ", ErrReservedName},
		{"github", nil},
		{"cron", nil},
		{"http", nil},
		{"manual", nil},
		{"Route", nil},
		{"route-a", nil},
		{"a-route", nil},
		{" 每晚体检 ", nil},
		{"name@v1", nil},
		{strings.Repeat("a", 64), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.name
			err := ValidateName(name)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateName(%q) = %v, want %v", name, err, tc.want)
			}
		})
	}
}
