//go:build linux

package main

import (
	"errors"
	"strings"
	"testing"
)

func TestAutostartEnabledOutputRequiresExactSuccess(t *testing.T) {
	for _, tc := range []struct {
		out  string
		err  error
		want bool
	}{
		{"enabled\n", nil, true},
		{"enabled-runtime\n", nil, false},
		{"enabled\n", errors.New("failed"), false},
		{"disabled\n", nil, false},
	} {
		if got := autostartEnabledOutput([]byte(tc.out), tc.err); got != tc.want {
			t.Errorf("output %q err=%v: got %v want %v", tc.out, tc.err, got, tc.want)
		}
	}
}

func TestSystemdUnitQuotesAndEscapesExecStart(t *testing.T) {
	unit := systemdUnit(`/home/user/My App/ccproxy\"service%$`)
	want := `ExecStart="/home/user/My App/ccproxy\\\"service%%$$" --daemon`
	if !strings.Contains(unit, want) {
		t.Fatalf("unit missing escaped ExecStart\nwant: %s\nunit:\n%s", want, unit)
	}
}
