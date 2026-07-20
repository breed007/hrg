package deliver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func mustNew(t *testing.T, typ, cfg, secret string) Destination {
	t.Helper()
	d, err := New(typ, json.RawMessage(cfg), secret)
	if err != nil {
		t.Fatalf("New(%s, %s): %v", typ, cfg, err)
	}
	return d
}

func TestFolderWritesFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "Home runbook")
	d := mustNew(t, "folder", `{"path":`+strconv.Quote(dir)+`}`, "")

	detail, err := d.Send(context.Background(), []File{
		{Name: "household-guide.pdf", Bytes: []byte("pdf bytes")},
		{Name: "administrator-guide.html", Bytes: []byte("<html>")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "2 file(s)") {
		t.Errorf("detail should say what was sent: %q", detail)
	}

	got, err := os.ReadFile(filepath.Join(dir, "household-guide.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pdf bytes" {
		t.Errorf("wrong content: %q", got)
	}
	// A runbook is a map of the network; it must not land world-readable.
	info, err := os.Stat(filepath.Join(dir, "household-guide.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("delivered file mode = %o, want 600", perm)
	}
}

// A path escape in Name must not let a file land outside the destination.
func TestFolderIgnoresPathsInNames(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dest")
	d := mustNew(t, "folder", `{"path":`+strconv.Quote(target)+`}`, "")

	if _, err := d.Send(context.Background(), []File{
		{Name: "../../escaped.pdf", Bytes: []byte("x")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.pdf")); err == nil {
		t.Error("file escaped the destination folder")
	}
	if _, err := os.Stat(filepath.Join(target, "escaped.pdf")); err != nil {
		t.Errorf("file not written under the destination: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name, typ, cfg, wantErr string
	}{
		{"folder needs a path", "folder", `{}`, "path is required"},
		{"folder rejects relative", "folder", `{"path":"exports"}`, "must be absolute"},
		{"email needs a host", "email", `{"port":"587","user":"u","from":"a@b.c","to":"d@e.f"}`, "host is required"},
		{"email rejects a bad port", "email", `{"host":"h","port":"smtp","user":"u","from":"a@b.c","to":"d@e.f"}`, "port must be a number"},
		{"email rejects a bad from", "email", `{"host":"h","port":"587","user":"u","from":"not an address","to":"d@e.f"}`, "from address"},
		{"email rejects no usable recipient", "email", `{"host":"h","port":"587","user":"u","from":"a@b.c","to":"nope"}`, "no valid recipients"},
		{"rclone needs a remote", "rclone", `{}`, "remote path is required"},
		{"rclone wants a remote name", "rclone", `{"remote":"just/a/path"}`, "no remote name"},
		{"rclone rejects a relative binary", "rclone", `{"remote":"box:x","binary":"./rclone"}`, "absolute path"},
		{"unknown type", "carrier-pigeon", `{}`, "unknown destination type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.typ, json.RawMessage(c.cfg), "")
			if err == nil {
				t.Fatalf("accepted invalid config %s", c.cfg)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not explain the problem (want %q)", err, c.wantErr)
			}
		})
	}
}

// The recipient list should survive one typo rather than failing entirely —
// but must still refuse to send when nothing usable is left.
func TestRecipientParsing(t *testing.T) {
	got := recipients(" a@example.com , garbage , b@example.com,")
	if len(got) != 2 || got[0] != "a@example.com" || got[1] != "b@example.com" {
		t.Errorf("recipients = %v", got)
	}
}

func TestEmailMessageStructure(t *testing.T) {
	d := mustNew(t, "email",
		`{"host":"smtp.example.com","port":"587","user":"u","from":"me@example.com","to":"you@example.com, them@example.com"}`,
		"hunter2").(*email)

	msg, err := d.compose(recipients(d.cfg.To), []File{
		{Name: "household-guide.pdf", ContentType: "application/pdf", Bytes: []byte("PDF")},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(msg)

	for _, want := range []string{
		"From: me@example.com",
		"To: you@example.com, them@example.com",
		"Content-Type: multipart/mixed;",
		`Content-Disposition: attachment; filename="household-guide.pdf"`,
		"Content-Transfer-Encoding: base64",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q", want)
		}
	}
	// The password must never appear in the message itself.
	if strings.Contains(got, "hunter2") {
		t.Error("SMTP password leaked into the message body")
	}
	// The body has to mean something to someone who has never heard of HRG.
	if !strings.Contains(got, "Household Guide") {
		t.Error("email body does not tell the reader what to open")
	}
}

// Base64 payloads must be wrapped: unwrapped lines break strict MTAs.
func TestBase64LineLength(t *testing.T) {
	d := mustNew(t, "email",
		`{"host":"h","port":"587","user":"u","from":"a@b.co","to":"c@d.co"}`, "").(*email)
	big := make([]byte, 4096)
	msg, err := d.compose([]string{"c@d.co"}, []File{{Name: "x.pdf", Bytes: big}})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(msg), "\r\n") {
		if len(line) > 78 {
			t.Fatalf("line of %d chars exceeds the MIME limit", len(line))
		}
	}
}

func TestKindsAreOrderedAndComplete(t *testing.T) {
	ks := Kinds()
	if len(ks) != 3 {
		t.Fatalf("expected 3 destination types, got %d", len(ks))
	}
	// The folder is offered first on purpose: it covers Dropbox, OneDrive,
	// Drive and iCloud with no credentials to hold or expire.
	if ks[0].Type != "folder" {
		t.Errorf("first offered type = %q, want folder", ks[0].Type)
	}
	for _, k := range ks {
		if k.Label == "" || k.Blurb == "" || k.New == nil {
			t.Errorf("kind %q is missing UI metadata", k.Type)
		}
	}
}
