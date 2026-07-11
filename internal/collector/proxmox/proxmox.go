// Package proxmox collects nodes, VMs, LXCs, storage, backup jobs, and HA
// state from a Proxmox VE cluster (or standalone node) via an API token.
//
// The token needs only read access: create it with `pveum` and grant
// PVEAuditor on /. HRG never writes to Proxmox.
//
// Source IDs reuse Proxmox's own cluster-resource IDs, which are stable:
// "node/pve1", "qemu/104", "lxc/105", "storage/pve1/local-zfs",
// "backup/backup-f3a2c1".
package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/collector/fixture"
	"github.com/breed007/hrg/internal/model"
)

// Type is the registry name.
const Type = "proxmox"

// Config is the non-secret instance configuration.
type Config struct {
	// URL of the API endpoint, e.g. https://pve1.lan:8006
	URL string `json:"url"`
	// TokenID in user@realm!tokenid form. The token value is the secret.
	TokenID string `json:"token_id"`
	// InsecureTLS skips certificate verification — common for Proxmox's
	// default self-signed cert. Prefer installing a real cert.
	InsecureTLS bool `json:"insecure_tls,omitempty"`
	// FixtureDir, when set, replays recorded API responses instead of
	// calling URL. For demos and tests.
	FixtureDir string `json:"fixture_dir,omitempty"`
}

type Collector struct {
	instance string
	cfg      Config
	secret   string
	client   *http.Client
}

// Factory builds instances from stored specs; registered in cmd/hrg.
func Factory(spec collector.Spec) (collector.Collector, error) {
	var cfg Config
	if err := json.Unmarshal(spec.Config, &cfg); err != nil {
		return nil, fmt.Errorf("%s: bad config: %w", spec.Instance, err)
	}
	if cfg.FixtureDir == "" {
		if cfg.URL == "" {
			return nil, fmt.Errorf("%s: url is required", spec.Instance)
		}
		if cfg.TokenID == "" || spec.Secret == "" {
			return nil, fmt.Errorf("%s: token_id and token secret are required", spec.Instance)
		}
	}

	c := &Collector{instance: spec.Instance, cfg: cfg, secret: spec.Secret}
	if cfg.FixtureDir != "" {
		c.client = fixture.Client(cfg.FixtureDir)
		c.cfg.URL = "https://fixture.invalid"
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if cfg.InsecureTLS {
			transport.TLSClientConfig.InsecureSkipVerify = true
		}
		c.client = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	}
	return c, nil
}

func (c *Collector) Name() string { return c.instance }

// clusterResource is one entry from /cluster/resources — Proxmox returns
// nodes, guests, and storage in a single call.
type clusterResource struct {
	ID      string  `json:"id"`
	Type    string  `json:"type"`
	Node    string  `json:"node"`
	Name    string  `json:"name"`
	VMID    int     `json:"vmid"`
	Status  string  `json:"status"`
	MaxCPU  float64 `json:"maxcpu"`
	MaxMem  int64   `json:"maxmem"`
	MaxDisk int64   `json:"maxdisk"`
	Disk    int64   `json:"disk"`
	Storage string  `json:"storage"`
	Plugin  string  `json:"plugintype"`
	Shared  int     `json:"shared"`
	Uptime  int64   `json:"uptime"`
	Templ   int     `json:"template"`
	Tags    string  `json:"tags"`
	Pool    string  `json:"pool"`
}

type backupJob struct {
	ID       string `json:"id"`
	Enabled  int    `json:"enabled"`
	Schedule string `json:"schedule"`
	Storage  string `json:"storage"`
	Mode     string `json:"mode"`
	VMIDs    string `json:"vmid"` // comma-separated
	All      int    `json:"all"`
	Comment  string `json:"comment"`
}

type haResource struct {
	SID   string `json:"sid"` // "vm:104" or "ct:105"
	State string `json:"state"`
	Group string `json:"group"`
}

func (c *Collector) Collect(ctx context.Context) (collector.Result, error) {
	var res collector.Result

	var cluster []clusterResource
	if err := c.get(ctx, "/api2/json/cluster/resources", &cluster); err != nil {
		return res, err
	}

	// vmid -> source id, for wiring backup jobs and HA state to guests.
	guestByVMID := map[int]string{}
	guestAttrs := map[string]map[string]any{}

	for _, cr := range cluster {
		switch cr.Type {
		case "node":
			res.Resources = append(res.Resources, model.Resource{
				Kind:     model.KindHost,
				SourceID: cr.ID,
				Name:     cr.Node,
				Attrs: pruned(map[string]any{
					"status": cr.Status,
					"cpus":   cr.MaxCPU,
					"memory": humanBytes(cr.MaxMem),
				}),
			})

		case "qemu", "lxc":
			kind := model.KindVM
			if cr.Type == "lxc" {
				kind = model.KindLXC
			}
			attrs := pruned(map[string]any{
				"vmid":     cr.VMID,
				"status":   cr.Status,
				"node":     cr.Node,
				"cpus":     cr.MaxCPU,
				"memory":   humanBytes(cr.MaxMem),
				"disk":     humanBytes(cr.MaxDisk),
				"tags":     cr.Tags,
				"pool":     cr.Pool,
				"template": cr.Templ == 1,
			})
			res.Resources = append(res.Resources, model.Resource{
				Kind: kind, SourceID: cr.ID, Name: cr.Name, Attrs: attrs,
			})
			guestByVMID[cr.VMID] = cr.ID
			guestAttrs[cr.ID] = attrs
			res.Edges = append(res.Edges, model.Edge{
				Src:      model.Ref{SourceID: cr.ID},
				Dst:      model.Ref{SourceID: "node/" + cr.Node},
				Relation: model.RelRunsOn,
			})

		case "storage":
			res.Resources = append(res.Resources, model.Resource{
				Kind:     model.KindStorage,
				SourceID: cr.ID,
				Name:     cr.Storage,
				Attrs: pruned(map[string]any{
					"node":   cr.Node,
					"status": cr.Status,
					"plugin": cr.Plugin,
					"shared": cr.Shared == 1,
					"size":   humanBytes(cr.MaxDisk),
					"used":   humanBytes(cr.Disk),
				}),
			})
			res.Edges = append(res.Edges, model.Edge{
				Src:      model.Ref{SourceID: cr.ID},
				Dst:      model.Ref{SourceID: "node/" + cr.Node},
				Relation: model.RelAttachedTo,
			})
		}
	}

	var jobs []backupJob
	if err := c.get(ctx, "/api2/json/cluster/backup", &jobs); err != nil {
		return res, err
	}
	for _, j := range jobs {
		name := "vzdump " + j.Schedule
		if j.Comment != "" {
			name = j.Comment
		}
		covers := "selected guests"
		if j.All == 1 {
			covers = "all guests"
		}
		res.Resources = append(res.Resources, model.Resource{
			Kind:     model.KindBackupJob,
			SourceID: "backup/" + j.ID,
			Name:     name,
			Attrs: pruned(map[string]any{
				"schedule": j.Schedule,
				"storage":  j.Storage,
				"mode":     j.Mode,
				"enabled":  j.Enabled == 1,
				"covers":   covers,
			}),
		})
		for vmid := range strings.SplitSeq(j.VMIDs, ",") {
			guest, ok := guestByVMID[atoi(strings.TrimSpace(vmid))]
			if !ok {
				continue // job references a vmid that no longer exists
			}
			res.Edges = append(res.Edges, model.Edge{
				Src:      model.Ref{SourceID: guest},
				Dst:      model.Ref{SourceID: "backup/" + j.ID},
				Relation: model.RelBackedUpBy,
			})
		}
	}

	var ha []haResource
	if err := c.get(ctx, "/api2/json/cluster/ha/resources", &ha); err != nil {
		return res, err
	}
	for _, h := range ha {
		_, vmid, ok := strings.Cut(h.SID, ":")
		if !ok {
			continue
		}
		if guest, ok := guestByVMID[atoi(vmid)]; ok {
			guestAttrs[guest]["ha_state"] = h.State
			if h.Group != "" {
				guestAttrs[guest]["ha_group"] = h.Group
			}
		}
	}

	return res, nil
}

// get calls a Proxmox API path and decodes the "data" envelope.
func (c *Collector) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.cfg.TokenID+"="+c.secret)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: GET %s: %w", c.instance, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: GET %s: %s", c.instance, path, resp.Status)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("%s: GET %s: decode: %w", c.instance, path, err)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("%s: GET %s: decode data: %w", c.instance, path, err)
	}
	return nil
}

// pruned drops zero-value entries so attrs stay tidy and hashes don't churn
// on absent-vs-empty differences.
func pruned(attrs map[string]any) map[string]any {
	for k, v := range attrs {
		switch t := v.(type) {
		case string:
			if t == "" {
				delete(attrs, k)
			}
		case bool:
			if !t {
				delete(attrs, k)
			}
		case float64:
			if t == 0 {
				delete(attrs, k)
			}
		case int:
			if t == 0 {
				delete(attrs, k)
			}
		}
	}
	return attrs
}

func humanBytes(b int64) string {
	if b <= 0 {
		return ""
	}
	const gib = 1 << 30
	if b >= gib {
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	}
	return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}
