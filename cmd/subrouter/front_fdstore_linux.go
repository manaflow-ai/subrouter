package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

var systemdNotifySocketSequence atomic.Uint64

const systemdNotifyBarrierTimeout = 5 * time.Second

func storeFrontListener(listener net.Listener) error {
	name, err := frontListenerStoreName(listener.Addr())
	if err != nil {
		return err
	}
	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		return fmt.Errorf("front descriptor store listener is %T, want TCP listener", listener)
	}
	file, err := duplicateTCPListenerFile(tcpListener)
	if err != nil {
		return fmt.Errorf("duplicate front listener for systemd descriptor store: %w", err)
	}
	defer file.Close()
	notified, err := notifySystemdDescriptorStore("FDSTORE=1\nFDNAME="+name, file)
	if err != nil {
		return err
	}
	if !notified {
		return nil
	}
	return nil
}

func removeStoredFrontListener(address net.Addr) error {
	name, err := frontListenerStoreName(address)
	if err != nil {
		return err
	}
	notified, err := notifySystemdDescriptorStore("FDSTOREREMOVE=1\nFDNAME="+name, nil)
	if err != nil || !notified {
		return err
	}
	barrierNotified, err := notifySystemdBarrier(systemdNotifyBarrierTimeout)
	if err != nil {
		return fmt.Errorf("wait for systemd descriptor removal: %w", err)
	}
	if !barrierNotified {
		return errors.New("systemd notification socket disappeared before descriptor removal was acknowledged")
	}
	return nil
}

func frontListenerStoreName(address net.Addr) (string, error) {
	port, err := frontListenerDescriptorStoreSlot(address)
	if err != nil {
		return "", fmt.Errorf("front listener address for descriptor store is invalid: %w", err)
	}
	return "subrouter-front-listener-" + port, nil
}

func notifySystemdDescriptorStore(message string, file *os.File) (bool, error) {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return false, nil
	}
	if strings.HasPrefix(path, "@") {
		path = "\x00" + path[1:]
	} else if !strings.HasPrefix(path, "/") {
		return true, fmt.Errorf("systemd notification socket is not absolute or abstract")
	}
	address := &net.UnixAddr{Name: path, Net: "unixgram"}
	localAddress := &net.UnixAddr{
		Name: fmt.Sprintf("\x00subrouter-notify-%d-%d", os.Getpid(), systemdNotifySocketSequence.Add(1)),
		Net:  "unixgram",
	}
	connection, err := net.ListenUnixgram("unixgram", localAddress)
	if err != nil {
		return true, fmt.Errorf("open systemd notification socket: %w", err)
	}
	defer connection.Close()
	if file == nil {
		written, _, writeErr := connection.WriteMsgUnix([]byte(message), nil, address)
		if writeErr != nil {
			return true, fmt.Errorf("notify systemd descriptor store: %w", writeErr)
		}
		if written != len(message) {
			return true, errors.New("notify systemd descriptor store: short write")
		}
		return true, nil
	}
	written, controlWritten, err := connection.WriteMsgUnix(
		[]byte(message), unix.UnixRights(int(file.Fd())), address,
	)
	if err != nil {
		return true, fmt.Errorf("send systemd notification descriptor: %w", err)
	}
	if written != len(message) || controlWritten == 0 {
		return true, errors.New("send systemd notification descriptor: incomplete message")
	}
	return true, nil
}

func notifySystemdBarrier(timeout time.Duration) (bool, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return true, fmt.Errorf("open systemd notification barrier: %w", err)
	}
	defer readEnd.Close()
	if err := readEnd.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		_ = writeEnd.Close()
		return true, fmt.Errorf("set systemd notification barrier deadline: %w", err)
	}
	notified, notifyErr := notifySystemdDescriptorStore("BARRIER=1", writeEnd)
	closeErr := writeEnd.Close()
	if notifyErr != nil {
		return notified, notifyErr
	}
	if closeErr != nil {
		return notified, fmt.Errorf("close systemd notification barrier sender: %w", closeErr)
	}
	if !notified {
		return false, nil
	}
	payload := []byte{0}
	if _, err := readEnd.Read(payload); !errors.Is(err, io.EOF) {
		if err == nil {
			return true, errors.New("systemd notification barrier returned data")
		}
		return true, fmt.Errorf("wait for systemd notification barrier: %w", err)
	}
	return true, nil
}
