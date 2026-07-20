package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

type rcloneConfig struct {
	Remote string `json:"remote"`
	Binary string `json:"binary"`
}

// rclone shells out to an already-configured rclone remote. HRG holds no
// credentials for it: rclone owns the OAuth dance and the token refresh,
// which is exactly the part that rots. That makes this the right answer
// for Box, S3, Backblaze, WebDAV and the dozens of others rclone speaks —
// and the wrong answer for anyone who hasn't already set rclone up.
type rclone struct {
	remote string
	binary string
}

func init() {
	Register(Kind{
		Type:  "rclone",
		Label: "rclone remote",
		Blurb: "Push to anything rclone can reach — Box, S3, Backblaze, WebDAV, and most of " +
			"the rest. rclone keeps the credentials and refreshes the tokens; HRG just " +
			"hands it files. Requires rclone already installed and configured.",
		Fields: []Field{
			{Key: "remote", Label: "Remote path", Required: true, Placeholder: "box:Home/runbook",
				Help: "As you'd type it in `rclone copy` — the remote name, a colon, then the folder."},
			{Key: "binary", Label: "rclone binary", Placeholder: "rclone",
				Help: "Leave blank unless rclone isn't on this machine's PATH."},
		},
		New: func(cfg json.RawMessage, _ string) (Destination, error) {
			var c rcloneConfig
			if err := json.Unmarshal(cfg, &c); err != nil {
				return nil, err
			}
			c.Remote = strings.TrimSpace(c.Remote)
			if c.Remote == "" {
				return nil, fmt.Errorf("rclone: remote path is required")
			}
			if !strings.Contains(c.Remote, ":") {
				return nil, fmt.Errorf("rclone: remote %q has no remote name — expected something like box:Home/runbook", c.Remote)
			}
			c.Binary = strings.TrimSpace(c.Binary)
			if c.Binary == "" {
				c.Binary = "rclone"
			}
			// A configured binary must be a bare command name or an
			// absolute path — never a relative path resolved against
			// whatever directory the server happens to be running in.
			if strings.ContainsRune(c.Binary, os.PathSeparator) && !filepath.IsAbs(c.Binary) {
				return nil, fmt.Errorf("rclone: binary must be a command name or an absolute path, got %q", c.Binary)
			}
			return &rclone{remote: c.Remote, binary: c.Binary}, nil
		},
	})
}

func (r *rclone) Send(ctx context.Context, files []File) (string, error) {
	bin, err := exec.LookPath(r.binary)
	if err != nil {
		return "", fmt.Errorf("rclone not found (%s): %w", r.binary, err)
	}
	// Stage the files under real names so they arrive under real names.
	dir, err := os.MkdirTemp("", "hrg-rclone-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	for _, f := range files {
		name := filepath.Base(f.Name)
		if err := os.WriteFile(filepath.Join(dir, name), f.Bytes, 0o600); err != nil {
			return "", err
		}
	}

	// copyto on the directory, so the remote ends up with exactly these
	// files under these names rather than a nested temp directory.
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "copy", dir, r.remote)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("rclone copy to %s: %s", r.remote, lastLine(msg))
	}
	return fmt.Sprintf("%d file(s) → %s", len(files), path.Clean(r.remote)), nil
}

// lastLine keeps error detail readable — rclone is chatty on failure and
// the actionable part is at the end.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
