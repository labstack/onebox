package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maxDockerResponse = 32 << 20

// Client is a minimal read-only Docker API client. Keeping the client local
// makes the controller's authority auditable: it has no methods for create,
// start, exec, logs, archives, volumes, images, or secrets.
type Client struct {
	http *http.Client
}

func NewDockerClient(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: transport}}
}

func (c *Client) Containers(ctx context.Context, application string) ([]Container, error) {
	filters, err := json.Marshal(map[string][]string{
		"label": {"com.docker.compose.project=" + application},
	})
	if err != nil {
		return nil, err
	}
	var summaries []struct {
		ID string `json:"Id"`
	}
	if err := c.getJSON(ctx, "/containers/json?all=0&filters="+url.QueryEscape(string(filters)), &summaries); err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]Container, 0, len(summaries))
	for _, summary := range summaries {
		container, err := c.inspect(ctx, summary.ID)
		if err != nil {
			// A container may disappear between list and inspect. A rebuild on
			// the corresponding event will settle the document; other failures
			// are returned so the last known-good file remains in place.
			if strings.Contains(err.Error(), "status 404") {
				continue
			}
			return nil, err
		}
		out = append(out, container)
	}
	return out, nil
}

func (c *Client) inspect(ctx context.Context, id string) (Container, error) {
	var raw struct {
		ID      string `json:"Id"`
		Created string `json:"Created"`
		Config  struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Status string `json:"Status"`
			Health *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress         string `json:"IPAddress"`
				GlobalIPv6Address string `json:"GlobalIPv6Address"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", &raw); err != nil {
		return Container{}, fmt.Errorf("inspect container %s: %w", shortID(id), err)
	}
	created, err := time.Parse(time.RFC3339Nano, raw.Created)
	if err != nil {
		return Container{}, fmt.Errorf("inspect container %s: invalid creation time: %w", shortID(id), err)
	}
	container := Container{
		ID: raw.ID, Created: created, Running: raw.State.Status == "running",
		Labels: raw.Config.Labels, Networks: map[string]string{},
	}
	if raw.State.Health != nil {
		container.Health = raw.State.Health.Status
	}
	for name, endpoint := range raw.NetworkSettings.Networks {
		address := endpoint.IPAddress
		if net.ParseIP(address) == nil {
			address = endpoint.GlobalIPv6Address
		}
		container.Networks[name] = address
	}
	return container, nil
}

func (c *Client) getJSON(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("docker API status %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDockerResponse)).Decode(target); err != nil {
		return fmt.Errorf("decode Docker response: %w", err)
	}
	return nil
}

// Events blocks until Docker emits a container event or the stream fails. The
// event payload is deliberately discarded: every signal causes a full rebuild,
// so event loss, ordering, rename, and health transitions converge to observed
// state rather than being replayed as mutations.
func (c *Client) Events(ctx context.Context, application string, since time.Time, trigger func() error) error {
	filters, err := json.Marshal(map[string][]string{
		"type":  {"container"},
		"label": {"com.docker.compose.project=" + application},
	})
	if err != nil {
		return err
	}
	values := url.Values{}
	values.Set("filters", string(filters))
	values.Set("since", fmt.Sprint(since.Unix()))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/events?"+values.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker events status %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if err := trigger(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func WriteAtomic(filename string, document Dynamic) error {
	body, err := yaml.Marshal(document)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(filename), ".onebox-routes-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filename)
}
