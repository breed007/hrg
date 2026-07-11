package runbook

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CommitTree commits the markdown tree in dir if dir is inside a git
// repository. Shelling out to git keeps the dependency surface at zero for
// everyone who doesn't use the feature; anyone who wants runbook history
// in git has git installed.
//
// Returns a human-readable result for the export log. Only a genuinely
// broken git invocation is an error — "not a repo" and "nothing changed"
// are normal outcomes.
func CommitTree(ctx context.Context, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	git := func(args ...string) (string, int, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		code := cmd.ProcessState.ExitCode()
		if err != nil && code < 0 {
			return "", code, err // git missing, context timeout, …
		}
		return strings.TrimSpace(string(out)), code, nil
	}

	if _, err := exec.LookPath("git"); err != nil {
		return "git not installed — commit skipped", nil
	}
	if out, code, err := git("rev-parse", "--is-inside-work-tree"); err != nil || code != 0 || out != "true" {
		return "not a git repository — run `git init` in the markdown export directory to get runbook history", nil
	}
	if out, code, err := git("add", "-A", "."); err != nil || code != 0 {
		return "", fmt.Errorf("git add: %s", out)
	}
	// Exit 0 = nothing staged, 1 = changes staged.
	if _, code, err := git("diff", "--cached", "--quiet"); err == nil && code == 0 {
		return "no changes since last commit", nil
	}
	msg := "HRG runbook update " + time.Now().UTC().Format("2006-01-02 15:04 MST")
	if out, code, err := git("commit", "-m", msg); err != nil || code != 0 {
		return "", fmt.Errorf("git commit: %s", out)
	}
	sha, _, _ := git("rev-parse", "--short", "HEAD")
	return "committed " + sha, nil
}
