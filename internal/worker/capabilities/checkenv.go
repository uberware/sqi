// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

type osCheckEnv struct{}

// OSCheckEnv returns the production CheckEnv backed by real OS queries.
func OSCheckEnv() CheckEnv { return osCheckEnv{} }

func (osCheckEnv) LookPath(file string) (string, bool) {
	p, err := exec.LookPath(file)
	return p, err == nil
}

func (osCheckEnv) Glob(pattern string) []string {
	m, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	return m
}

func (osCheckEnv) Getenv(key string) (string, bool) { return os.LookupEnv(key) }

func (osCheckEnv) GOOS() string { return runtime.GOOS }

func (osCheckEnv) RegistryExists(key string) bool { return registryExists(key) }
