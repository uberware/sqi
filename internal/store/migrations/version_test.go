// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store/migrations"
)

func TestMaxVersion_IsTheSameOnBothDrivers(t *testing.T) {
	s, err := migrations.MaxVersion("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	p, err := migrations.MaxVersion("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if s != p || s < 37 {
		t.Fatalf("MaxVersion sqlite %d, postgres %d; want equal and >= 37", s, p)
	}
}

func TestCheckVersion(t *testing.T) {
	maxV, err := migrations.MaxVersion("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []int64{0, 1, maxV} {
		if err := migrations.CheckVersion("sqlite", v); err != nil {
			t.Errorf("CheckVersion(%d) = %v, want nil", v, err)
		}
	}
	err = migrations.CheckVersion("sqlite", maxV+1)
	if !errors.Is(err, migrations.ErrSchemaTooNew) {
		t.Fatalf("CheckVersion(max+1) = %v, want ErrSchemaTooNew", err)
	}
	var tooNew *migrations.SchemaTooNewError
	if !errors.As(err, &tooNew) || tooNew.DBVersion != maxV+1 || tooNew.BinaryVersion != maxV {
		t.Fatalf("error = %#v, want both versions", err)
	}
	for _, want := range []string{"newer than this sqi-server", "restoring the pre-upgrade backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
}

func TestMaxVersion_UnknownDriver(t *testing.T) {
	if _, err := migrations.MaxVersion("mysql"); err == nil {
		t.Fatal("MaxVersion(mysql) = nil error, want one")
	}
}
