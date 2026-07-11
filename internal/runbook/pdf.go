package runbook

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// chromeCandidates are the binaries tried, in order, for PDF rendering.
// A PATH lookup covers Linux (the container ships Chromium); the absolute
// paths cover macOS dev machines.
func chromeCandidates() []string {
	names := []string{
		"chromium", "chromium-browser", "google-chrome",
		"google-chrome-stable", "chrome",
	}
	if runtime.GOOS == "darwin" {
		names = append(names,
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		)
	}
	return names
}

// FindChrome returns the first usable Chrome/Chromium binary, or "" if none
// is installed. Callers surface a clear "install Chromium for PDF" message
// rather than failing hard — PDF is optional; HTML and Markdown are not.
func FindChrome() string {
	for _, name := range chromeCandidates() {
		if filepath.IsAbs(name) {
			if info, err := os.Stat(name); err == nil && !info.IsDir() {
				return name
			}
			continue
		}
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// ErrNoChrome signals that PDF export was requested but no browser is
// available to render it.
type ErrNoChrome struct{}

func (ErrNoChrome) Error() string {
	return "no Chrome/Chromium found — PDF export needs a headless browser (the Docker image ships one; on a bare install, `apt install chromium` or set the browser on PATH). HTML and Markdown exports do not need it."
}

// RenderPDF renders the HTML artifact to PDF via headless Chrome's
// --print-to-pdf. Chrome reads the HTML from a file:// URL so the print
// stylesheet and inlined assets apply exactly as they do on screen.
func RenderPDF(ctx context.Context, chrome string, htmlDoc []byte) ([]byte, error) {
	if chrome == "" {
		return nil, ErrNoChrome{}
	}

	// Chrome needs real files: an HTML input it can load and a PDF output
	// path it writes. A private temp dir keeps the network map off any
	// shared location.
	dir, err := os.MkdirTemp("", "hrg-pdf-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "runbook.html")
	outPath := filepath.Join(dir, "runbook.pdf")
	if err := os.WriteFile(inPath, htmlDoc, 0o600); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, chrome,
		"--headless",
		"--no-sandbox", // required when running as root in a container
		"--disable-gpu",
		"--no-pdf-header-footer",
		// The topology diagram renders via async Mermaid JS. Without a
		// time budget, --print-to-pdf waits for network/JS idle that never
		// arrives and hangs. This advances Chrome's virtual clock and
		// prints once it elapses — plenty for Mermaid, and it completes in
		// seconds of wall-clock regardless.
		"--virtual-time-budget=15000",
		"--run-all-compositor-stages-before-draw",
		// Quiet the branded-Chrome background services (updater, sync,
		// component updates) that otherwise linger after the render.
		"--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--disable-component-update",
		"--disable-sync", "--disable-default-apps", "--disable-extensions",
		"--user-data-dir="+filepath.Join(dir, "profile"),
		"--print-to-pdf="+outPath,
		"file://"+inPath,
	)

	// Chrome's output goes to a file, not a pipe (a forked grandchild can
	// hold a pipe open and stall exec.Wait).
	logPath := filepath.Join(dir, "chrome.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	// Some Chrome builds (notably the branded Google Chrome) write the PDF
	// but never exit, kept alive by background services. So don't wait for
	// exit: poll for the output file and, once its size holds steady, take
	// it and kill the process. Chrome writes the PDF atomically at the end
	// of the render, so a stable non-zero size means "done".
	poll := time.NewTicker(300 * time.Millisecond)
	defer poll.Stop()
	var lastSize int64 = -1
	stable := 0

	finish := func() ([]byte, error) {
		pdf, err := os.ReadFile(outPath)
		if err != nil {
			return nil, fmt.Errorf("chrome produced no PDF: %w", err)
		}
		if len(pdf) == 0 {
			return nil, fmt.Errorf("chrome produced an empty PDF")
		}
		return pdf, nil
	}

	for {
		select {
		case err := <-waitErr:
			// Chrome exited on its own (well-behaved Chromium path).
			if err != nil && ctx.Err() == nil {
				chromeLog, _ := os.ReadFile(logPath)
				return nil, fmt.Errorf("chrome: %w: %s", err, chromeLog)
			}
			if ctx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("PDF render timed out (Mermaid diagram too large?)")
			}
			return finish()

		case <-ctx.Done():
			cmd.Process.Kill()
			<-waitErr
			return nil, fmt.Errorf("PDF render timed out (Mermaid diagram too large?)")

		case <-poll.C:
			info, err := os.Stat(outPath)
			if err != nil || info.Size() == 0 {
				continue
			}
			if info.Size() == lastSize {
				stable++
				if stable >= 2 { // ~600ms unchanged: the render is done
					cmd.Process.Kill()
					<-waitErr
					return finish()
				}
			} else {
				stable = 0
				lastSize = info.Size()
			}
		}
	}
}
