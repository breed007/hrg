package deliver

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type emailConfig struct {
	Host string `json:"host"`
	Port string `json:"port"`
	User string `json:"user"`
	From string `json:"from"`
	To   string `json:"to"`
}

// email attaches the guides to a message. This is the only destination
// that keeps working after every machine in the house is gone — which is
// the scenario the Household Guide exists for. It is also the one a
// non-technical recipient can actually use: the attachment is just there,
// in a place they already look.
type email struct {
	cfg  emailConfig
	pass string
}

func init() {
	Register(Kind{
		Type:  "email",
		Label: "Email",
		Blurb: "Send the guides as attachments. The one destination that still works when " +
			"every machine in the house is gone — and the one a non-technical person can " +
			"find without being told how.",
		NeedsSecret: true,
		SecretLabel: "SMTP password",
		Fields: []Field{
			{Key: "host", Label: "SMTP server", Required: true, Placeholder: "smtp.fastmail.com"},
			{Key: "port", Label: "Port", Required: true, Placeholder: "587",
				Help: "587 for STARTTLS (usual), 465 for implicit TLS."},
			{Key: "user", Label: "Username", Required: true, Placeholder: "you@example.com"},
			{Key: "from", Label: "From", Required: true, Placeholder: "you@example.com"},
			{Key: "to", Label: "To", Required: true, Placeholder: "partner@example.com, you@example.com",
				Help: "Comma-separated. Send it to the person who would need it, not only to yourself."},
		},
		New: func(cfg json.RawMessage, secret string) (Destination, error) {
			var c emailConfig
			if err := json.Unmarshal(cfg, &c); err != nil {
				return nil, err
			}
			for k, v := range map[string]string{
				"host": c.Host, "port": c.Port, "user": c.User, "from": c.From, "to": c.To,
			} {
				if strings.TrimSpace(v) == "" {
					return nil, fmt.Errorf("email: %s is required", k)
				}
			}
			if _, err := strconv.Atoi(c.Port); err != nil {
				return nil, fmt.Errorf("email: port must be a number, got %q", c.Port)
			}
			if _, err := mail.ParseAddress(c.From); err != nil {
				return nil, fmt.Errorf("email: from address %q: %w", c.From, err)
			}
			if len(recipients(c.To)) == 0 {
				return nil, fmt.Errorf("email: no valid recipients in %q", c.To)
			}
			return &email{cfg: c, pass: secret}, nil
		},
	})
}

// recipients splits and validates the To list, dropping anything unusable
// rather than failing the whole delivery over one typo.
func recipients(to string) []string {
	var out []string
	for _, part := range strings.Split(to, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if a, err := mail.ParseAddress(part); err == nil {
			out = append(out, a.Address)
		}
	}
	return out
}

func (e *email) Send(ctx context.Context, files []File) (string, error) {
	to := recipients(e.cfg.To)
	msg, err := e.compose(to, files)
	if err != nil {
		return "", err
	}

	addr := net.JoinHostPort(e.cfg.Host, e.cfg.Port)
	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("connect %s: %w", addr, err)
	}
	// Port 465 is implicit TLS (the handshake happens before SMTP); every
	// other port speaks plain SMTP first and upgrades with STARTTLS.
	if e.cfg.Port == "465" {
		conn = tls.Client(conn, &tls.Config{ServerName: e.cfg.Host})
	}

	c, err := smtp.NewClient(conn, e.cfg.Host)
	if err != nil {
		conn.Close()
		return "", fmt.Errorf("smtp handshake: %w", err)
	}
	defer c.Close()

	if e.cfg.Port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: e.cfg.Host}); err != nil {
				return "", fmt.Errorf("starttls: %w", err)
			}
		} else {
			// Refuse to push a map of the network across the wire in the
			// clear, even on a LAN relay.
			return "", fmt.Errorf("%s does not offer STARTTLS — refusing to send the runbook unencrypted", addr)
		}
	}

	if e.pass != "" {
		if err := c.Auth(smtp.PlainAuth("", e.cfg.User, e.pass, e.cfg.Host)); err != nil {
			return "", fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(e.cfg.From); err != nil {
		return "", fmt.Errorf("smtp from: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return "", fmt.Errorf("smtp rcpt %s: %w", rcpt, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return "", fmt.Errorf("smtp data: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		wc.Close()
		return "", fmt.Errorf("smtp write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return "", fmt.Errorf("smtp close: %w", err)
	}
	if err := c.Quit(); err != nil {
		return "", fmt.Errorf("smtp quit: %w", err)
	}
	return fmt.Sprintf("%d file(s) → %s", len(files), strings.Join(to, ", ")), nil
}

// compose builds a multipart/mixed message. The body is written for the
// recipient, who may have no idea why this keeps arriving — so it says
// what the attachment is and what to do with it.
func (e *email) compose(to []string, files []File) ([]byte, error) {
	boundary := "hrg-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var b strings.Builder

	fmt.Fprintf(&b, "From: %s\r\n", e.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8",
		"Home runbook — "+time.Now().Format("January 2, 2006")))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	// SMTP wants CRLF throughout; a bare LF in DATA is the kind of thing
	// that works against one server and is mangled by the next.
	b.WriteString(strings.ReplaceAll(bodyText, "\n", "\r\n"))
	b.WriteString("\r\n")

	for _, f := range files {
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		ct := f.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		name := filepath.Base(f.Name)
		fmt.Fprintf(&b, "Content-Type: %s; name=%q\r\n", ct, name)
		fmt.Fprintf(&b, "Content-Disposition: attachment; filename=%q\r\n", name)
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString(wrap76(base64.StdEncoding.EncodeToString(f.Bytes)))
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String()), nil
}

// bodyText is addressed to whoever opens this in five years, possibly
// under bad circumstances, possibly having never heard of HRG.
const bodyText = `Attached is the current guide to the technology in the house.

Keep it. If something stops working — the internet, the TVs, anything
that beeps — open the Household Guide first. It is written in plain
language and assumes you have never touched any of it.

If you are handing this to someone technical, the Administrator Guide
is the one they will want.

This message is generated automatically. Nobody typed it.
`

// wrap76 hard-wraps base64 to the line length MIME requires.
func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76])
		b.WriteString("\r\n")
		s = s[76:]
	}
	b.WriteString(s)
	return b.String()
}
