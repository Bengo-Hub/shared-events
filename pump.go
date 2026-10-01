package events

import (
	"context"
	"time"
)

// Socket is the minimum a realtime connection needs for Pump, expressed as functions so this
// package does not depend on any WebSocket library. With nhooyr.io/websocket:
//
//	events.Socket{
//		Read:  func(ctx context.Context) ([]byte, error) { _, b, err := conn.Read(ctx); return b, err },
//		Write: func(ctx context.Context, b []byte) error { return conn.Write(ctx, websocket.MessageText, b) },
//		Ping:  conn.Ping,
//	}
type Socket struct {
	Read  func(ctx context.Context) ([]byte, error)
	Write func(ctx context.Context, b []byte) error
	Ping  func(ctx context.Context) error
}

const (
	// PumpWriteTimeout bounds every frame write, so a stalled client cannot hold the loop.
	PumpWriteTimeout = 5 * time.Second
	// PumpPingInterval keeps idle connections alive through proxies and detects dead peers.
	PumpPingInterval = 25 * time.Second
)

// Pump serves one connection from a FanoutHub subscription until the client disconnects or ctx
// ends: it writes hello first (when non-nil), then every message from sub.C, pings every
// PumpPingInterval, and passes each client frame to reply; a non-nil return is written back
// (hubs use it to answer a JSON "ping" with "pong"). Every write has PumpWriteTimeout.
//
// This is the one write loop for every hub in the fleet; each service only adapts its socket.
func Pump(ctx context.Context, sock Socket, sub *Sub, hello []byte, reply func(frame []byte) []byte) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	replies := make(chan []byte, 4)
	go func() {
		defer cancel()
		for {
			frame, err := sock.Read(ctx)
			if err != nil {
				return
			}
			if reply == nil {
				continue
			}
			if out := reply(frame); out != nil {
				select {
				case replies <- out:
				default: // client is spamming; drop
				}
			}
		}
	}()

	write := func(b []byte) error {
		wctx, wcancel := context.WithTimeout(ctx, PumpWriteTimeout)
		defer wcancel()
		return sock.Write(wctx, b)
	}
	if hello != nil {
		if err := write(hello); err != nil {
			return
		}
	}
	ticker := time.NewTicker(PumpPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-sub.C:
			if !ok {
				return
			}
			if err := write(msg); err != nil {
				return
			}
		case out := <-replies:
			if err := write(out); err != nil {
				return
			}
		case <-ticker.C:
			if sock.Ping == nil {
				continue
			}
			pctx, pcancel := context.WithTimeout(ctx, PumpWriteTimeout)
			err := sock.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}
