package mime

import (
	"fmt"
	stdmime "mime"
	"os"
	"path/filepath"
)

// FileAttachment reads a file off the disk as something a draft can carry:
// named for its last path element, typed by its extension. It is what
// `mail send --attach` and the TUI composer's ctrl+o both attach with, so a
// file means the same thing whichever of them put it on the message.
func FileAttachment(path string) (DraftAttachment, error) {
	info, err := os.Stat(path)
	if err != nil {
		return DraftAttachment{}, err
	}
	if info.IsDir() {
		return DraftAttachment{}, fmt.Errorf("%s is a directory", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DraftAttachment{}, err
	}
	ct := stdmime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return DraftAttachment{Filename: filepath.Base(path), ContentType: ct, Data: data}, nil
}
