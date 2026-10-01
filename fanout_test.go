package events

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func recv(t *testing.T, c <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-c:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
		return nil
	}
}

func none(t *testing.T, c <-chan []byte) {
	t.Helper()
	select {
	case b := <-c:
		t.Fatalf("unexpected message %s", b)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestFanoutHubAcrossReplicasScopesAndTenants(t *testing.T) {
	ncA := runNATS(t)
	ncB, err := nats.Connect(ncA.ConnectedUrl())
	if err != nil {
		t.Fatal(err)
	}
	defer ncB.Close()
	podA, _ := NewFanoutHub(NewBroadcaster(nil, ncA, "pos"), "notif", 8)
	podB, _ := NewFanoutHub(NewBroadcaster(nil, ncB, "pos"), "notif", 8)
	_ = ncA.Flush()
	_ = ncB.Flush()

	alice := podB.Subscribe("t1", "user:alice", "outlet:o1")
	bob := podB.Subscribe("t1", "user:bob")
	other := podB.Subscribe("t2", "user:alice")
	local := podA.Subscribe("t1")

	podA.Publish("t1", "", []byte("all"))
	if string(recv(t, alice.C)) != "all" || string(recv(t, bob.C)) != "all" || string(recv(t, local.C)) != "all" {
		t.Fatal("tenant-wide message must reach every t1 subscriber on both replicas")
	}
	none(t, other.C)
	none(t, local.C) // exactly once on the publishing replica

	podA.Publish("t1", "user:alice", []byte("dm"))
	if string(recv(t, alice.C)) != "dm" {
		t.Fatal("scoped message must reach the scoped subscriber")
	}
	none(t, bob.C)
	none(t, other.C)

	podB.Unsubscribe(alice)
	if _, ok := <-alice.C; ok {
		t.Fatal("unsubscribe must close the channel")
	}
	podB.Unsubscribe(alice) // idempotent
}

func TestFanoutHubDropsWhenFull(t *testing.T) {
	h, _ := NewFanoutHub(nil, "x", 1)
	s := h.Subscribe("t")
	h.Publish("t", "", []byte("1"))
	h.Publish("t", "", []byte("2"))
	if s.Dropped.Load() != 1 {
		t.Fatalf("dropped=%d, want 1", s.Dropped.Load())
	}
}

func TestFanoutHubWildcardScope(t *testing.T) {
	h, _ := NewFanoutHub(nil, "kds", 4)
	all := h.Subscribe("t", WildcardScope)
	one := h.Subscribe("t", "outlet:o1")
	h.Publish("t", "outlet:o2", []byte("x"))
	if string(recv(t, all.C)) != "x" {
		t.Fatal("wildcard subscriber must receive every scoped message")
	}
	none(t, one.C)
}

// fakeSocket records writes and serves scripted client frames.
type fakeSocket struct {
	in     chan []byte
	writes chan []byte
}

func (f *fakeSocket) sock() Socket {
	return Socket{
		Read: func(ctx context.Context) ([]byte, error) {
			select {
			case b, ok := <-f.in:
				if !ok {
					return nil, context.Canceled
				}
				return b, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		Write: func(ctx context.Context, b []byte) error { f.writes <- b; return nil },
		Ping:  func(context.Context) error { return nil },
	}
}

func TestPumpHelloMessagesRepliesAndClose(t *testing.T) {
	h, _ := NewFanoutHub(nil, "t", 4)
	sub := h.Subscribe("t1")
	fs := &fakeSocket{in: make(chan []byte, 2), writes: make(chan []byte, 8)}
	done := make(chan struct{})
	go func() {
		Pump(context.Background(), fs.sock(), sub, []byte("hello"), func(b []byte) []byte {
			if string(b) == "ping" {
				return []byte("pong")
			}
			return nil
		})
		close(done)
	}()
	if string(recv(t, fs.writes)) != "hello" {
		t.Fatal("hello must be written first")
	}
	h.Publish("t1", "", []byte("msg"))
	if string(recv(t, fs.writes)) != "msg" {
		t.Fatal("hub message must be written")
	}
	fs.in <- []byte("ping")
	if string(recv(t, fs.writes)) != "pong" {
		t.Fatal("reply must be written")
	}
	close(fs.in) // client disconnects
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump must return when the client disconnects")
	}
}
