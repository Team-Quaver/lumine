package wl

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFDPassingRoundTrip(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	send, recv := pair[0], pair[1]
	defer unix.Close(send)
	defer unix.Close(recv)

	f, err := os.CreateTemp(t.TempDir(), "wlfd")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("lumine"); err != nil {
		t.Fatal(err)
	}

	if _, err := unix.SendmsgN(send, []byte("hi"), packFDs([]int{int(f.Fd())}), nil, 0); err != nil {
		t.Fatalf("sendmsg: %v", err)
	}
	buf := make([]byte, 64)
	oob := make([]byte, 128)
	_, oobn, _, _, err := unix.Recvmsg(recv, buf, oob, 0)
	if err != nil {
		t.Fatalf("recvmsg: %v", err)
	}
	got := unpackFDs(oob[:oobn])
	if len(got) != 1 {
		t.Fatalf("expected 1 fd, got %v", got)
	}
	defer unix.Close(got[0])
	data := make([]byte, 16)
	m, err := unix.Pread(got[0], data, 0)
	if err != nil {
		t.Fatalf("pread: %v", err)
	}
	if string(data[:m]) != "lumine" {
		t.Fatalf("fd contents = %q", data[:m])
	}
}
