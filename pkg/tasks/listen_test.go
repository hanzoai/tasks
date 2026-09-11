//go:build darwin || linux

// Copyright © 2026 Hanzo AI. MIT License.

package tasks_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/tasks"
)

// TestZAPClientsListenNowhere starts a Tasks server on loopback and asks the
// operating system which sockets this process listens on. The server adds one,
// which shows the probe can see a listener at all. The tasks client and the SDK
// client each call the server over the connection they dialled, and add none.
func TestZAPClientsListenNowhere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	before := listening(t)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	emb, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: addr})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	t.Cleanup(func() { _ = emb.Stop(context.Background()) })

	withServer := listening(t)
	if added := addedTo(withServer, before); len(added) != 1 {
		t.Fatalf("probe saw %v for the server's one listener", added)
	}

	c := tasks.New(addr, nil)
	t.Cleanup(c.Stop)
	if err := c.Now("listen.probe", map[string]any{"n": 1}); err != nil {
		t.Fatalf("Now: %v", err)
	}
	// Now runs the task in-process when the ZAP submit fails, so the server's
	// record is what shows the submit went over ZAP.
	rows, err := emb.View(tasks.Principal{}).ListWorkflows("default")
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	submitted := false
	for i := range rows {
		submitted = submitted || rows[i].Type.Name == "listen.probe"
	}
	if !submitted {
		t.Fatal("server has no record of the workflow submitted over ZAP")
	}

	cli, err := client.Dial(client.Options{Address: addr, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	if _, err := cli.CheckHealth(ctx, nil); err != nil {
		t.Fatalf("health over ZAP: %v", err)
	}

	if added := addedTo(listening(t), withServer); len(added) > 0 {
		t.Fatalf("the ZAP clients listen on %v", added)
	}
}

// listening is every socket this process listens on, by descriptor, as the
// operating system reports it. A stream socket with no peer is one that
// listens: macOS does not answer SO_ACCEPTCONN, and a stream socket Go has
// finished dialling has a peer.
func listening(t *testing.T) map[int]string {
	t.Helper()
	// Names only: the descriptor reading the directory is closed by the time
	// its entry would be stat'ed.
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatalf("open descriptors: %v", err)
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		t.Fatalf("list descriptors: %v", err)
	}
	out := make(map[int]string)
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		if typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err != nil || typ != syscall.SOCK_STREAM {
			continue // not a socket, or not a stream
		}
		if _, err := syscall.Getpeername(fd); err != syscall.ENOTCONN {
			continue // connected
		}
		sa, err := syscall.Getsockname(fd)
		if err != nil {
			continue
		}
		out[fd] = sockaddr(sa)
	}
	return out
}

func sockaddr(sa syscall.Sockaddr) string {
	switch a := sa.(type) {
	case *syscall.SockaddrInet4:
		return net.JoinHostPort(net.IP(a.Addr[:]).String(), strconv.Itoa(a.Port))
	case *syscall.SockaddrInet6:
		return net.JoinHostPort(net.IP(a.Addr[:]).String(), strconv.Itoa(a.Port))
	case *syscall.SockaddrUnix:
		return a.Name
	}
	return fmt.Sprintf("%T", sa)
}

// addedTo is what now holds that before did not.
func addedTo(now, before map[int]string) []string {
	var added []string
	for fd, addr := range now {
		if was, ok := before[fd]; !ok || was != addr {
			added = append(added, addr)
		}
	}
	return added
}
