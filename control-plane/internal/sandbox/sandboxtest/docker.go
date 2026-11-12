// Package sandboxtest is a fake Docker daemon for tests, whose containers
// run a scripted stand-in for Pi's RPC mode behind attach.
package sandboxtest

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Docker is the fake daemon. Its containers answer prompts with "echo:
// <message>"; a prompt holding "tools" first lists the tools at the sandbox
// API, as Pi's extension does, and reports how many it got.
type Docker struct {
	Server *httptest.Server

	mu         sync.Mutex
	Containers map[string]*Container // by name
	Volumes    map[string]bool
	Created    int
	Missing    bool // the image is not there until pulled
	Pulled     []string
}

// Container is a fake container.
type Container struct {
	ID, Name, Image string
	Env             []string
	Labels          map[string]string
	Running         bool
	Removed         bool
	messages        int
}

func (c *Container) env(k string) string {
	for _, e := range c.Env {
		if v, ok := strings.CutPrefix(e, k+"="); ok {
			return v
		}
	}
	return ""
}

// New starts a fake daemon.
func New(t *testing.T) *Docker {
	d := &Docker{Containers: map[string]*Container{}, Volumes: map[string]bool{}}
	d.Server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.Server.Close)
	return d
}

// Addr is the daemon's host:port.
func (d *Docker) Addr() string { return strings.TrimPrefix(d.Server.URL, "http://") }

// Running is the number of running containers.
func (d *Docker) Running() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.Containers {
		if c.Running && !c.Removed {
			n++
		}
	}
	return n
}

func (d *Docker) find(ref string) *Container {
	for _, c := range d.Containers {
		if !c.Removed && (c.ID == ref || c.Name == ref) {
			return c
		}
	}
	return nil
}

func (d *Docker) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	d.mu.Lock()
	switch {
	case r.Method == "POST" && p == "/volumes/create":
		var b struct{ Name string }
		json.NewDecoder(r.Body).Decode(&b)
		d.Volumes[b.Name] = true
		d.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "{}")
	case r.Method == "POST" && p == "/images/create":
		d.Pulled = append(d.Pulled, r.URL.Query().Get("fromImage")+":"+r.URL.Query().Get("tag"))
		d.Missing = false
		d.mu.Unlock()
		io.WriteString(w, `{"status":"Pulled"}`)
	case r.Method == "POST" && p == "/containers/create":
		var b struct {
			Image  string
			Env    []string
			Labels map[string]string
		}
		json.NewDecoder(r.Body).Decode(&b)
		if d.Missing {
			d.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such image"}`)
			return
		}
		name := r.URL.Query().Get("name")
		if d.find(name) != nil {
			d.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"message":"name in use"}`)
			return
		}
		d.Created++
		c := &Container{ID: fmt.Sprintf("c%d", d.Created), Name: name, Image: b.Image, Env: b.Env, Labels: b.Labels}
		d.Containers[name+"#"+c.ID] = c
		d.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":%q}`, c.ID)
	case r.Method == "GET" && p == "/containers/json":
		var out []map[string]any
		for _, c := range d.Containers {
			if !c.Removed {
				out = append(out, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "Labels": c.Labels})
			}
		}
		d.mu.Unlock()
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, "/containers/"):
		ref, action, _ := strings.Cut(strings.TrimPrefix(p, "/containers/"), "/")
		c := d.find(ref)
		if c == nil {
			d.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such container"}`)
			return
		}
		switch {
		case r.Method == "DELETE":
			c.Removed, c.Running = true, false
			d.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case action == "start":
			c.Running = true
			d.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case action == "attach":
			d.mu.Unlock()
			d.attach(w, c)
		default:
			d.mu.Unlock()
			io.WriteString(w, "")
		}
	default:
		d.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"not faked"}`)
	}
}

// attach hijacks the connection and plays Pi until stdin ends.
func (d *Docker) attach(w http.ResponseWriter, c *Container) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	var wmu sync.Mutex
	emit := func(v any) {
		b, _ := json.Marshal(v)
		b = append(b, '\n')
		var head [8]byte
		head[0] = 1
		binary.BigEndian.PutUint32(head[4:], uint32(len(b)))
		wmu.Lock()
		conn.Write(head[:])
		conn.Write(b)
		wmu.Unlock()
	}
	in := bufio.NewReader(rw)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var cmd struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		json.Unmarshal(line, &cmd)
		d.mu.Lock()
		dead := c.Removed
		n := c.messages
		d.mu.Unlock()
		if dead {
			return
		}
		switch cmd.Type {
		case "get_state":
			emit(map[string]any{"type": "response", "id": cmd.ID, "command": "get_state", "success": true, "data": map[string]any{"messageCount": n}})
		case "abort":
			emit(map[string]any{"type": "response", "id": cmd.ID, "command": "abort", "success": true})
		case "prompt":
			emit(map[string]any{"type": "response", "id": cmd.ID, "command": "prompt", "success": true})
			emit(map[string]any{"type": "agent_start"})
			answer := "echo: " + cmd.Message
			if strings.Contains(cmd.Message, "tools") {
				emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{map[string]any{"type": "text", "text": "Let me look."}}}})
				emit(map[string]any{"type": "tool_execution_start", "toolName": "list_repos", "args": map[string]any{"namespace": "homepage"}})
				answer = d.listTools(c)
				emit(map[string]any{"type": "tool_execution_end", "toolName": "list_repos", "isError": false})
			}
			emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": "hmm"}})
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": answer}}}})
			emit(map[string]any{"type": "agent_end", "messages": []any{}})
			d.mu.Lock()
			c.messages += 2
			d.mu.Unlock()
		}
	}
}

func (d *Docker) listTools(c *Container) string {
	req, _ := http.NewRequest("GET", c.env("HOMECLOUD_API")+"/tools", nil)
	req.Header.Set("Authorization", "Bearer "+c.env("HOMECLOUD_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "tools: " + err.Error()
	}
	defer resp.Body.Close()
	var specs []map[string]any
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&specs) != nil {
		return fmt.Sprintf("tools: %s", resp.Status)
	}
	return fmt.Sprintf("tools: %d", len(specs))
}
