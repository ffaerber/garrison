package sandbox_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"homecloud/control-plane/internal/sandbox"
	"homecloud/control-plane/internal/sandbox/sandboxtest"
)

func manager(t *testing.T) (*sandbox.Manager, *sandboxtest.Docker) {
	d := sandboxtest.New(t)
	return &sandbox.Manager{Docker: &sandbox.Docker{Addr: d.Addr()}, Image: "reg/system/sandbox:2", Network: "sandboxes", API: "http://agents", Memory: 1 << 30, CPUs: 1, Pids: 100}, d
}

func ask(t *testing.T, m *sandbox.Manager, want sandbox.Want, msg string) string {
	t.Helper()
	var answer string
	err := m.Turn(context.Background(), want, sandbox.Run{Token: "kc-token"}, func(s *sandbox.Session) error {
		return s.Pi().Prompt(context.Background(), msg, nil, func(ev sandbox.Event) {
			if ev.Type == "message_end" && ev.Message.StopReason == "stop" {
				answer = ev.Message.Text()
			}
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

func TestASandboxIsReused(t *testing.T) {
	m, d := manager(t)
	w := sandbox.Want{Key: "chat\x00a/b", Agent: "a/b", Person: "alice", Definition: "1"}
	if got := ask(t, m, w, "one"); got != "echo: one" {
		t.Fatal(got)
	}
	ask(t, m, w, "two")
	if d.Created != 1 || !d.Volumes[sandbox.Name(w.Key)] {
		t.Errorf("created %d, volumes %v", d.Created, d.Volumes)
	}
}

func TestTheImageIsPulledWhenMissing(t *testing.T) {
	m, d := manager(t)
	d.Missing = true
	ask(t, m, sandbox.Want{Key: "k", Person: "alice"}, "hi")
	if len(d.Pulled) != 1 || d.Pulled[0] != "reg/system/sandbox:2" {
		t.Errorf("pulled %v", d.Pulled)
	}
}

func TestANewImageRestartsTheSandbox(t *testing.T) {
	m, d := manager(t)
	w := sandbox.Want{Key: "k", Person: "alice"}
	ask(t, m, w, "hi")
	m.Image = "reg/system/sandbox:3"
	ask(t, m, w, "hi")
	if d.Created != 2 || d.Running() != 1 {
		t.Errorf("created %d, running %d", d.Created, d.Running())
	}
}

func TestAPersonsOldestIdleSandboxMakesRoom(t *testing.T) {
	m, d := manager(t)
	m.PerPerson = 2
	for _, k := range []string{"a", "b", "c"} {
		ask(t, m, sandbox.Want{Key: k, Person: "alice"}, "hi")
	}
	ask(t, m, sandbox.Want{Key: "d", Person: "bob"}, "hi")
	if d.Running() != 3 || len(m.Running()) != 3 {
		t.Errorf("running %d", d.Running())
	}
	for _, w := range m.Running() {
		if w.Key == "a" {
			t.Error("alice's oldest sandbox still runs")
		}
	}
}

func TestAllBusyIsRefused(t *testing.T) {
	m, _ := manager(t)
	m.PerPerson = 1
	err := m.Turn(context.Background(), sandbox.Want{Key: "a", Person: "alice"}, sandbox.Run{Token: "kc"}, func(*sandbox.Session) error {
		return m.Turn(context.Background(), sandbox.Want{Key: "b", Person: "alice"}, sandbox.Run{Token: "kc"}, func(*sandbox.Session) error { return nil })
	})
	if !errors.Is(err, sandbox.ErrBusy) {
		t.Errorf("err %v", err)
	}
}

func TestCleanRemovesEarlierContainers(t *testing.T) {
	m, d := manager(t)
	ask(t, m, sandbox.Want{Key: "a", Person: "alice"}, "hi")
	m2 := &sandbox.Manager{Docker: m.Docker, Image: m.Image}
	if err := m2.Clean(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.Running() != 0 {
		t.Errorf("running %d", d.Running())
	}
}

func TestUnderGVisorASandboxIsRootAndReachedByAddress(t *testing.T) {
	m, d := manager(t)
	m.Runtime, m.API, m.Proxy = "runsc", "http://agents:8071", "http://agents:8072"
	ask(t, m, sandbox.Want{Key: "k", Person: "alice"}, "hi")
	for _, c := range d.Containers {
		if c.Runtime != "runsc" || c.User != "0:0" {
			t.Errorf("runtime %q, user %q", c.Runtime, c.User)
		}
		// gVisor has no Docker DNS: the agent service by its address on the
		// network (the fake's is loopback).
		if got := c.Env[0]; got != "HOMECLOUD_API=http://127.0.0.1:8071" {
			t.Errorf("%s", got)
		}
		var proxy, noProxy string
		for _, e := range c.Env {
			if v, ok := strings.CutPrefix(e, "HTTPS_PROXY="); ok {
				proxy = v
			}
			if v, ok := strings.CutPrefix(e, "NO_PROXY="); ok {
				noProxy = v
			}
		}
		if !strings.HasPrefix(proxy, "http://sandbox:") || !strings.HasSuffix(proxy, "@127.0.0.1:8072") || noProxy != "127.0.0.1" {
			t.Errorf("proxy %q, no proxy %q", proxy, noProxy)
		}
	}
}

func TestWithoutARuntimeASandboxHasNoCapabilities(t *testing.T) {
	m, d := manager(t)
	ask(t, m, sandbox.Want{Key: "k", Person: "alice"}, "hi")
	for _, c := range d.Containers {
		if c.Runtime != "" || c.User != "" || c.Env[0] != "HOMECLOUD_API=http://agents" {
			t.Errorf("runtime %q, user %q, %s", c.Runtime, c.User, c.Env[0])
		}
	}
}

func TestOwnAddress(t *testing.T) {
	_, sandboxes, _ := net.ParseCIDR("10.0.7.0/24")
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.ParseIP("10.0.1.5"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("10.0.7.3"), Mask: net.CIDRMask(24, 32)},
	}
	if got := sandbox.OwnAddress(addrs, []*net.IPNet{sandboxes}); got != "10.0.7.3" {
		t.Errorf("%q", got)
	}
	if got := sandbox.OwnAddress(addrs[:2], []*net.IPNet{sandboxes}); got != "" {
		t.Errorf("%q", got)
	}
}
