// Package docker collects containers, compose projects, volumes, and
// networks from a Docker Engine API endpoint (unix socket or TCP).
//
// Source-ID strategy — the hard part of any Docker collector: container IDs
// churn on every recreate, so they are useless as identity. Compose-managed
// containers use "compose/{project}/{service}" (stable across recreates,
// which is exactly when you want annotations to survive); everything else
// falls back to "container/{name}". Compose projects themselves become
// service resources grouping their containers.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/collector/fixture"
	"github.com/breed007/hrg/internal/model"
)

// Type is the registry name.
const Type = "docker"

// Config is the non-secret instance configuration. Docker has no token;
// access control is the socket/endpoint itself.
type Config struct {
	// Host is the Engine API endpoint: unix:///var/run/docker.sock,
	// tcp://host:2375, or http(s)://host:2376.
	Host string `json:"host"`
	// FixtureDir, when set, replays recorded API responses. For demos/tests.
	FixtureDir string `json:"fixture_dir,omitempty"`
}

type Collector struct {
	instance string
	baseURL  string
	client   *http.Client
}

// Factory builds instances from stored specs; registered in cmd/hrg.
func Factory(spec collector.Spec) (collector.Collector, error) {
	var cfg Config
	if err := json.Unmarshal(spec.Config, &cfg); err != nil {
		return nil, fmt.Errorf("%s: bad config: %w", spec.Instance, err)
	}

	c := &Collector{instance: spec.Instance}
	switch {
	case cfg.FixtureDir != "":
		c.client = fixture.Client(cfg.FixtureDir)
		c.baseURL = "http://docker"

	case strings.HasPrefix(cfg.Host, "unix://"):
		socket := strings.TrimPrefix(cfg.Host, "unix://")
		c.client = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		}
		c.baseURL = "http://docker"

	case strings.HasPrefix(cfg.Host, "tcp://"):
		c.baseURL = "http://" + strings.TrimPrefix(cfg.Host, "tcp://")
		c.client = &http.Client{Timeout: 30 * time.Second}

	case strings.HasPrefix(cfg.Host, "http://"), strings.HasPrefix(cfg.Host, "https://"):
		c.baseURL = cfg.Host
		c.client = &http.Client{Timeout: 30 * time.Second}

	case cfg.Host == "":
		return nil, fmt.Errorf("%s: host is required (e.g. unix:///var/run/docker.sock)", spec.Instance)

	default:
		return nil, fmt.Errorf("%s: host must start with unix://, tcp://, http:// or https://", spec.Instance)
	}
	return c, nil
}

func (c *Collector) Name() string { return c.instance }

type apiContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		IP          string `json:"IP"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct{} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type apiVolume struct {
	Name       string `json:"Name"`
	Driver     string `json:"Driver"`
	Mountpoint string `json:"Mountpoint"`
}

type apiNetwork struct {
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
}

func (c *Collector) Collect(ctx context.Context) (collector.Result, error) {
	var res collector.Result

	var containers []apiContainer
	if err := c.get(ctx, "/containers/json", url.Values{"all": {"1"}}, &containers); err != nil {
		return res, err
	}
	var volumes struct {
		Volumes []apiVolume `json:"Volumes"`
	}
	if err := c.get(ctx, "/volumes", nil, &volumes); err != nil {
		return res, err
	}
	var networks []apiNetwork
	if err := c.get(ctx, "/networks", nil, &networks); err != nil {
		return res, err
	}

	for _, n := range networks {
		res.Resources = append(res.Resources, model.Resource{
			Kind:     model.KindNetwork,
			SourceID: "network/" + n.Name,
			Name:     n.Name,
			Attrs:    map[string]any{"driver": n.Driver},
		})
	}
	for _, v := range volumes.Volumes {
		res.Resources = append(res.Resources, model.Resource{
			Kind:     model.KindStorage,
			SourceID: "volume/" + v.Name,
			Name:     v.Name,
			Attrs:    map[string]any{"driver": v.Driver, "mountpoint": v.Mountpoint},
		})
	}

	projects := map[string]bool{}
	for _, ct := range containers {
		name := containerName(ct)
		project := ct.Labels["com.docker.compose.project"]
		service := ct.Labels["com.docker.compose.service"]

		sourceID := "container/" + name
		attrs := map[string]any{
			"image":  ct.Image,
			"state":  ct.State,
			"status": ct.Status,
		}
		if project != "" && service != "" {
			sourceID = "compose/" + project + "/" + service
			attrs["compose_project"] = project
			attrs["compose_service"] = service
			if !projects[project] {
				projects[project] = true
				res.Resources = append(res.Resources, model.Resource{
					Kind:     model.KindService,
					SourceID: "project/" + project,
					Name:     project,
					Attrs:    map[string]any{"compose_project": true},
				})
			}
			res.Edges = append(res.Edges, model.Edge{
				Src:      model.Ref{SourceID: sourceID},
				Dst:      model.Ref{SourceID: "project/" + project},
				Relation: model.RelMemberOf,
			})
		}
		if ports := formatPorts(ct); len(ports) > 0 {
			attrs["ports"] = ports
		}

		res.Resources = append(res.Resources, model.Resource{
			Kind:     model.KindContainer,
			SourceID: sourceID,
			Name:     name,
			Attrs:    attrs,
		})

		for _, m := range ct.Mounts {
			if m.Type == "volume" && m.Name != "" {
				res.Edges = append(res.Edges, model.Edge{
					Src:      model.Ref{SourceID: sourceID},
					Dst:      model.Ref{SourceID: "volume/" + m.Name},
					Relation: model.RelAttachedTo,
				})
			}
		}
		for _, netName := range sortedKeys(ct.NetworkSettings.Networks) {
			res.Edges = append(res.Edges, model.Edge{
				Src:      model.Ref{SourceID: sourceID},
				Dst:      model.Ref{SourceID: "network/" + netName},
				Relation: model.RelMemberOf,
			})
		}
	}

	return res, nil
}

func containerName(ct apiContainer) string {
	if len(ct.Names) > 0 {
		return strings.TrimPrefix(ct.Names[0], "/")
	}
	if len(ct.ID) >= 12 {
		return ct.ID[:12]
	}
	return ct.ID
}

// formatPorts renders port mappings like "0.0.0.0:8080→80/tcp", deduplicated
// (the API repeats mappings for IPv4/IPv6) and sorted for hash stability.
func formatPorts(ct apiContainer) []string {
	set := map[string]bool{}
	for _, p := range ct.Ports {
		var s string
		if p.PublicPort != 0 {
			ip := p.IP
			if ip == "::" {
				continue // IPv6 twin of the 0.0.0.0 entry
			}
			s = fmt.Sprintf("%s:%d→%d/%s", ip, p.PublicPort, p.PrivatePort, p.Type)
		} else {
			s = fmt.Sprintf("%d/%s (not published)", p.PrivatePort, p.Type)
		}
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (c *Collector) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: GET %s: %w", c.instance, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: GET %s: %s", c.instance, path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: GET %s: decode: %w", c.instance, path, err)
	}
	return nil
}
