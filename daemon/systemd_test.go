//go:build linux

package daemon

import (
	"strings"
	"testing"
)

func TestSystemdUnitContainsBoundedFailurePolicy(t *testing.T) {
	unit := (&systemdManager{system: true}).buildUnit(Config{
		BinaryPath: "/usr/local/bin/pi-connect",
		WorkDir:    "/srv/pi-connect",
		LogFile:    "/var/log/pi-connect.log",
	})

	for _, directive := range []string{
		"Restart=on-failure\n",
		"OOMPolicy=continue\n",
		"KillMode=control-group\n",
	} {
		if strings.Count(unit, directive) != 1 {
			t.Fatalf("unit directive %q count = %d, want 1\n%s", strings.TrimSpace(directive), strings.Count(unit, directive), unit)
		}
	}
	for _, forbidden := range []string{"MemoryHigh=", "MemoryMax=", "OOMScoreAdjust=", "TasksMax="} {
		if strings.Contains(unit, forbidden) {
			t.Fatalf("unit unexpectedly contains %q\n%s", forbidden, unit)
		}
	}
}
