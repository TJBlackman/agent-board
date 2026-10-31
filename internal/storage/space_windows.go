//go:build windows

package storage

import "errors"

func freeBytes(dir string) (uint64, error) { return 0, errors.New("not probed on windows") }
