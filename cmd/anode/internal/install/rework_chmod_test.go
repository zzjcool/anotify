package install

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReworkInstallChmodFailureExactCompensation(t *testing.T) {
	for _, fault := range []string{"config", "unit", "user-replaced-binary", "user-edited-binary"} {
		t.Run(fault, func(t *testing.T) {
			o := fixture(t)
			if err := o.defaults(); err != nil {
				t.Fatal(err)
			}
			oldChmod := chmodTemporary
			t.Cleanup(func() { chmodTemporary = oldChmod })
			faultDir := filepath.Dir(o.ConfigPath)
			if fault == "unit" {
				faultDir = filepath.Dir(o.UnitPath)
			}
			var committed bool
			var replaced []byte
			chmodTemporary = func(f *os.File, mode os.FileMode) error {
				if filepath.Dir(f.Name()) != faultDir {
					return oldChmod(f, mode)
				}
				binary, err := os.ReadFile(o.BinPath)
				source, sourceErr := os.ReadFile(o.Source)
				if err != nil || sourceErr != nil || !bytes.Equal(binary, source) {
					t.Fatalf("chmod fault must occur after actual binary commit: %v %v", err, sourceErr)
				}
				committed = true
				if fault == "user-replaced-binary" || fault == "user-edited-binary" {
					replaced = []byte("user replacement: preserve")
					tmp := filepath.Join(filepath.Dir(o.BinPath), "user-replacement")
					if err := os.WriteFile(tmp, replaced, 0700); err != nil {
						t.Fatal(err)
					}
					if fault == "user-edited-binary" {
						if err := os.WriteFile(o.BinPath, replaced, 0700); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Rename(tmp, o.BinPath); err != nil {
						t.Fatal(err)
					}
				}
				return os.ErrPermission
			}
			err := Apply(o, &bytes.Buffer{})
			if !committed || !errors.Is(err, os.ErrPermission) {
				t.Fatalf("Apply did not hit precise chmod failure after binary commit: %v", err)
			}
			chmodTemporary = oldChmod
			if replaced != nil {
				got, err := os.ReadFile(o.BinPath)
				if err != nil || !bytes.Equal(got, replaced) {
					t.Fatalf("compensation deleted user-replaced file: %q %v", got, err)
				}
				if err := Apply(o, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not an established") {
					t.Fatalf("unknown user binary claimed as partial install: %v", err)
				}
			} else {
				for _, path := range []string{o.BinPath, o.ConfigPath, o.UnitPath} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("chmod failure left this invocation's new file %s: %v", path, err)
					}
				}
				if err := Apply(o, &bytes.Buffer{}); err != nil {
					t.Fatalf("cleared chmod fault cannot safely reinstall: %v", err)
				}
			}
			for _, p := range []string{o.Prefix, o.DataPath, faultDir} {
				if st, err := os.Stat(p); err != nil || !st.IsDir() {
					t.Fatalf("compensation removed directory %s: %v", p, err)
				}
			}
		})
	}
}
