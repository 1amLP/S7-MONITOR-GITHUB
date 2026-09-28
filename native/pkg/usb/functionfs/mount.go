package functionfs

import (
	"os"
	"os/exec"
)

func Mount(name string, path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0755); err != nil {
			return err
		}
	}

	// Check if already mounted
	// For simplicity, we just try to mount. If it's already mounted, it might return an error which we can ignore.
	cmd := exec.Command("mount", "-t", "functionfs", name, path)
	return cmd.Run()
}
