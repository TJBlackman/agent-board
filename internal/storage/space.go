package storage

import (
	"fmt"
	"os"
)

// checkSpace fails when the backup directory has less free space than twice
// the live database size (plus a small floor).
func checkSpace(dir, dbPath string) error {
	info, err := os.Stat(dbPath)
	if err != nil {
		return err
	}
	need := uint64(info.Size())*2 + 64<<20
	free, err := freeBytes(dir)
	if err != nil {
		return nil // platforms without a probe skip the check
	}
	if free < need {
		return fmt.Errorf("insufficient free space for backup: need %d bytes, have %d", need, free)
	}
	return nil
}
