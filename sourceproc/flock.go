package sourceproc

import (
	"windows-m3u-stream-merger-proxy/config"
	"sync"

	"github.com/gofrs/flock"
)

var lockFile *flock.Flock
var lockFilePath string
var mu sync.Mutex

func init() {
	lockFilePath = config.GetLockFile()
	lockFile = flock.New(lockFilePath)
}

func ensureLockFile() {
	currentPath := config.GetLockFile()
	if lockFile == nil || lockFilePath != currentPath {
		lockFilePath = currentPath
		lockFile = flock.New(lockFilePath)
	}
}

func LockSources() {
	mu.Lock()
	defer mu.Unlock()

	ensureLockFile()
	_ = lockFile.Lock()
}

func UnlockSources() {
	mu.Lock()
	defer mu.Unlock()

	ensureLockFile()
	_ = lockFile.Unlock()
}

