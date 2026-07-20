package deliver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// folderConfig writes into a directory on this machine.
type folderConfig struct {
	Path string `json:"path"`
}

// folder is the highest-leverage destination in HRG: Dropbox, OneDrive,
// Google Drive and iCloud Drive all present themselves as a local folder,
// so one mechanism covers four services with no API, no OAuth, and no
// token to expire silently three years from now.
type folder struct{ path string }

func init() {
	Register(Kind{
		Type:  "folder",
		Label: "Sync folder",
		Blurb: "Write a copy into a folder on this machine. If that folder is your Dropbox, " +
			"OneDrive, Google Drive or iCloud Drive, their app takes it from there — no " +
			"passwords for HRG to hold and nothing to expire.",
		Fields: []Field{{
			Key: "path", Label: "Folder", Required: true,
			Placeholder: "/Users/you/Dropbox/Home runbook",
			Help: "Full path to a folder this machine can write to. It will be created if " +
				"it doesn't exist. Files are overwritten each time the runbook regenerates.",
		}},
		New: func(cfg json.RawMessage, _ string) (Destination, error) {
			var c folderConfig
			if err := json.Unmarshal(cfg, &c); err != nil {
				return nil, err
			}
			c.Path = strings.TrimSpace(c.Path)
			if c.Path == "" {
				return nil, fmt.Errorf("folder: path is required")
			}
			if !filepath.IsAbs(c.Path) {
				return nil, fmt.Errorf("folder: path must be absolute, got %q", c.Path)
			}
			return &folder{path: c.Path}, nil
		},
	})
}

func (f *folder) Send(_ context.Context, files []File) (string, error) {
	if err := os.MkdirAll(f.path, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", f.path, err)
	}
	// 0600: a runbook is a map of the network. Sync clients preserve the
	// content, not the mode, but there is no reason to be careless locally.
	for _, file := range files {
		dst := filepath.Join(f.path, filepath.Base(file.Name))
		if err := os.WriteFile(dst, file.Bytes, 0o600); err != nil {
			return "", fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return fmt.Sprintf("%d file(s) → %s", len(files), f.path), nil
}
