//go:build windows

package platform

import "os/exec"

// RevealInFileManager spawns Explorer with /select to highlight the
// file at path.
func RevealInFileManager(path string) error {
	return exec.Command("explorer", "/select,"+path).Start()
}
