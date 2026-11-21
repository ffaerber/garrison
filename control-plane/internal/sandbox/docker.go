// Package sandbox runs the agents' sandboxes: one container per chat, with
// Pi (the agent harness) as its main process, driven over stdin and stdout.
// Containers are started through the sandbox guard (internal/guard), which
// lets the agent service create nothing but sandboxes.
package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Docker is a client of the Docker API at a TCP address, e.g. the guard's.
type Docker struct {
	Addr string // host:port
	HTTP *http.Client
}

// ErrNotFound is Docker's 404.
var ErrNotFound = errors.New("not found")

func (d *Docker) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// do calls the API; out, if not nil, receives the JSON answer.
func (d *Docker) do(ctx context.Context, method, path string, query url.Values, header http.Header, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	u := "http://" + d.Addr + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("docker: %s %s: %w: %s", method, path, ErrNotFound, message(data))
	case resp.StatusCode == http.StatusNotModified:
		return nil // already started or stopped
	case resp.StatusCode >= 300:
		return fmt.Errorf("docker: %s %s: %s: %s", method, path, resp.Status, message(data))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func message(data []byte) string {
	var m struct{ Message string }
	if json.Unmarshal(data, &m) == nil && m.Message != "" {
		return m.Message
	}
	return strings.TrimSpace(string(data))
}

// Container is what Docker lists of a container.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// List lists the containers, running or not, that carry label.
func (d *Docker) List(ctx context.Context, label string) ([]Container, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []Container
	err := d.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(filters)}}, nil, nil, &out)
	return out, err
}

// Spec is a container to create.
type Spec struct {
	Name    string
	Image   string
	Env     []string
	Labels  map[string]string
	Network string
	Volume  string // mounted at /data
	Media   string // if set, a host folder bound at MediaTarget
	Memory  int64  // bytes
	CPUs    float64
	Pids    int64
	// Runtime, if set, e.g. runsc (gVisor). The container's processes then
	// run as root with Docker's default capabilities, inside gVisor's own
	// kernel; without it, as the image's user with no capabilities.
	Runtime string
}

// Create creates a container whose stdin stays open for attaching.
func (d *Docker) Create(ctx context.Context, s Spec) (string, error) {
	mounts := []map[string]any{{"Type": "volume", "Source": s.Volume, "Target": "/data"}}
	if s.Media != "" {
		// Where the share was on the host before shares became OpenEBS's
		// volumes; the Docker engine predates them.
		mounts = append(mounts, map[string]any{"Type": "bind", "Source": "/garrison/ns/" + s.Media + "/shares/media", "Target": MediaTarget})
	}
	host := map[string]any{
		"NetworkMode": s.Network,
		"Mounts":      mounts,
		"Memory":      s.Memory,
		"NanoCpus":    int64(s.CPUs * 1e9),
		"PidsLimit":   s.Pids,
		"Init":        true,
	}
	body := map[string]any{
		"Image": s.Image, "Env": s.Env, "Labels": s.Labels,
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true,
		"OpenStdin": true, "StdinOnce": false, "Tty": false,
		"HostConfig": host,
	}
	if s.Runtime != "" {
		host["Runtime"] = s.Runtime
		body["User"] = "0:0"
	} else {
		host["CapDrop"] = []string{"ALL"}
	}
	var out struct{ Id string }
	err := d.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {s.Name}}, nil, body, &out)
	return out.Id, err
}

// Subnets are the IP ranges of a network.
func (d *Docker) Subnets(ctx context.Context, network string) ([]*net.IPNet, error) {
	var n struct {
		IPAM struct{ Config []struct{ Subnet string } }
	}
	if err := d.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(network), nil, nil, nil, &n); err != nil {
		return nil, err
	}
	var out []*net.IPNet
	for _, c := range n.IPAM.Config {
		if _, ipn, err := net.ParseCIDR(c.Subnet); err == nil {
			out = append(out, ipn)
		}
	}
	return out, nil
}

// Pull pulls an image; auth is the X-Registry-Auth value, or empty.
func (d *Docker) Pull(ctx context.Context, image, auth string) error {
	h := http.Header{}
	if auth != "" {
		h.Set("X-Registry-Auth", auth)
	}
	// The answer is a stream of progress lines; an error shows in the last.
	name, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		name, tag = image[:i], image[i+1:]
	}
	u := "http://" + d.Addr + "/images/create?" + url.Values{"fromImage": {name}, "tag": {tag}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header = h
	resp, err := d.client().Do(req)
	if err != nil {
		return fmt.Errorf("docker: pull %s: %w", image, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("docker: pull %s: %s: %s", image, resp.Status, message(data))
	}
	if i := bytes.LastIndex(data, []byte(`"error"`)); i >= 0 {
		return fmt.Errorf("docker: pull %s: %s", image, strings.TrimSpace(string(data[max(0, i-1):])))
	}
	return nil
}

// CreateVolume creates a named local volume; an existing one is kept.
func (d *Docker) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return d.do(ctx, http.MethodPost, "/volumes/create", nil, nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

// RemoveVolume removes a volume.
func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	return d.do(ctx, http.MethodDelete, "/volumes/"+name, nil, nil, nil, nil)
}

func (d *Docker) Start(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil, nil)
}

// Remove stops and removes a container; a missing one is no error.
func (d *Docker) Remove(ctx context.Context, id string) error {
	err := d.do(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}}, nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Logs is the end of a container's stderr: why Pi stopped, if it did.
func (d *Docker) Logs(ctx context.Context, id string, tail int) (string, error) {
	u := "http://" + d.Addr + "/containers/" + id + "/logs?" + url.Values{"stdout": {"0"}, "stderr": {"1"}, "tail": {fmt.Sprint(tail)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	demux(io.LimitReader(resp.Body, 1<<20), io.Discard, &out)
	return out.String(), nil
}

// Attach connects to a container's stdin, stdout and stderr. It is called
// before the container starts, so nothing it writes is missed. Stdout comes
// out of the returned reader; stderr goes to errs.
func (d *Docker) Attach(ctx context.Context, id string, errs io.Writer) (io.WriteCloser, io.ReadCloser, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", d.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("docker: attach: %w", err)
	}
	q := url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	req, _ := http.NewRequest(http.MethodPost, "http://"+d.Addr+"/containers/"+id+"/attach?"+q.Encode(), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("docker: attach: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("docker: attach: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		conn.Close()
		return nil, nil, fmt.Errorf("docker: attach: %s: %s", resp.Status, message(data))
	}
	out, w := io.Pipe()
	go func() {
		w.CloseWithError(demux(br, w, errs))
	}()
	return halfCloser{conn}, readCloser{out, conn}, nil
}

type halfCloser struct{ net.Conn }

// Close ends stdin only; the container keeps writing.
func (h halfCloser) Close() error {
	if c, ok := h.Conn.(interface{ CloseWrite() error }); ok {
		return c.CloseWrite()
	}
	return h.Conn.Close()
}

type readCloser struct {
	io.Reader
	conn net.Conn
}

func (r readCloser) Close() error { return r.conn.Close() }

// demux splits Docker's multiplexed stream (a container without a TTY) into
// stdout and stderr. It returns at the stream's end.
func demux(r io.Reader, stdout, stderr io.Writer) error {
	var head [8]byte
	for {
		if _, err := io.ReadFull(r, head[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		n := int64(binary.BigEndian.Uint32(head[4:]))
		dst := stdout
		if head[0] == 2 {
			dst = stderr
		}
		if _, err := io.CopyN(dst, r, n); err != nil {
			return err
		}
	}
}
