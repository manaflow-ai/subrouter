package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStoreFrontListenerWithoutSystemdIsNoOp(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := storeFrontListener(listener); err != nil {
		t.Fatal(err)
	}
}

func TestFrontMainPIDNotificationWaitsForSystemdBarrier(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "subrouter-mainpid-barrier-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	notifyPath := filepath.Join(directory, "notify.sock")
	notifyAddress, err := net.ResolveUnixAddr("unixgram", notifyPath)
	if err != nil {
		t.Fatal(err)
	}
	notifyListener, err := net.ListenUnixgram("unixgram", notifyAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer notifyListener.Close()
	if err := notifyListener.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOTIFY_SOCKET", notifyPath)

	result := make(chan error, 1)
	go func() {
		notified, err := notifyFrontMainPID("4242")
		if err == nil && !notified {
			err = fmt.Errorf("MAINPID notification unexpectedly reported no systemd socket")
		}
		result <- err
	}()
	readNotification := func() (string, []int) {
		t.Helper()
		payload := make([]byte, 256)
		control := make([]byte, unix.CmsgSpace(4*4))
		payloadBytes, controlBytes, flags, _, err := notifyListener.ReadMsgUnix(payload, control)
		if err != nil {
			t.Fatal(err)
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			t.Fatal("systemd notification was truncated")
		}
		messages, err := unix.ParseSocketControlMessage(control[:controlBytes])
		if err != nil {
			t.Fatal(err)
		}
		var descriptors []int
		for _, message := range messages {
			rights, err := unix.ParseUnixRights(&message)
			if err != nil {
				t.Fatal(err)
			}
			descriptors = append(descriptors, rights...)
		}
		return string(payload[:payloadBytes]), descriptors
	}
	mainPIDMessage, mainPIDDescriptors := readNotification()
	if mainPIDMessage != "MAINPID=4242" || len(mainPIDDescriptors) != 0 {
		t.Fatalf("MAINPID notification = %q with descriptors %v", mainPIDMessage, mainPIDDescriptors)
	}
	barrierMessage, barrierDescriptors := readNotification()
	if barrierMessage != "BARRIER=1" || len(barrierDescriptors) != 1 {
		for _, descriptor := range barrierDescriptors {
			_ = unix.Close(descriptor)
		}
		t.Fatalf("barrier notification = %q with %d descriptors", barrierMessage, len(barrierDescriptors))
	}
	select {
	case err := <-result:
		_ = unix.Close(barrierDescriptors[0])
		t.Fatalf("MAINPID notification returned before barrier acknowledgement: %v", err)
	default:
	}
	if err := unix.Close(barrierDescriptors[0]); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MAINPID notification did not finish after barrier acknowledgement")
	}
}

func TestSystemdDescriptorStoreReceivesExactFrontListener(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "subrouter-fdstore-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	notifyPath := filepath.Join(directory, "notify.sock")
	notifyAddress, err := net.ResolveUnixAddr("unixgram", notifyPath)
	if err != nil {
		t.Fatal(err)
	}
	notifyListener, err := net.ListenUnixgram("unixgram", notifyAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer notifyListener.Close()
	if err := notifyListener.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOTIFY_SOCKET", notifyPath)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := storeFrontListener(listener); err != nil {
		t.Fatal(err)
	}

	readNotification := func() (string, []int) {
		t.Helper()
		payload := make([]byte, 256)
		control := make([]byte, unix.CmsgSpace(4*4))
		payloadBytes, controlBytes, flags, _, err := notifyListener.ReadMsgUnix(payload, control)
		if err != nil {
			t.Fatal(err)
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			t.Fatal("systemd descriptor notification was truncated")
		}
		messages, err := unix.ParseSocketControlMessage(control[:controlBytes])
		if err != nil {
			t.Fatal(err)
		}
		var descriptors []int
		for _, message := range messages {
			rights, err := unix.ParseUnixRights(&message)
			if err != nil {
				t.Fatal(err)
			}
			descriptors = append(descriptors, rights...)
		}
		return string(payload[:payloadBytes]), descriptors
	}
	storeMessage, storeDescriptors := readNotification()
	if !strings.Contains(storeMessage, "FDSTORE=1") || len(storeDescriptors) != 1 {
		t.Fatalf("store notification = %q with %d descriptors", storeMessage, len(storeDescriptors))
	}
	defer unix.Close(storeDescriptors[0])

	sourceFile, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer sourceFile.Close()
	var sourceStat unix.Stat_t
	var storedStat unix.Stat_t
	if err := unix.Fstat(int(sourceFile.Fd()), &sourceStat); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fstat(storeDescriptors[0], &storedStat); err != nil {
		t.Fatal(err)
	}
	if sourceStat.Ino != storedStat.Ino {
		t.Fatalf("stored listener inode = %d, want %d", storedStat.Ino, sourceStat.Ino)
	}
	if err := removeStoredFrontListener(listener.Addr()); err != nil {
		t.Fatal(err)
	}
	removeMessage, removeDescriptors := readNotification()
	if !strings.Contains(removeMessage, "FDSTOREREMOVE=1") || len(removeDescriptors) != 0 {
		t.Fatalf("remove notification = %q with descriptors %v", removeMessage, removeDescriptors)
	}
}
