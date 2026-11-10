package install

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestInstallRejectsShellControlInAdministratorSteps(t *testing.T) {
	for _, leaf := range []string{"stage'; echo unsafe #", "stage-$UNSET"} {
		o := fixture(t)
		o.Prefix = filepath.Join(filepath.Dir(o.Prefix), leaf)
		if err := Apply(o, &bytes.Buffer{}); err == nil {
			t.Fatalf("unsafe shell/systemd expansion in installation path accepted: %q", leaf)
		}
	}
}
